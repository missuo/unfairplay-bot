package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sentMessage struct {
	chat int64
	id   int
	text string
	kb   Keyboard
}

// fakeMessenger records what the bot sends and keeps each message's latest text.
type fakeMessenger struct {
	mu      sync.Mutex
	nextID  int
	msgs    map[int]*sentMessage
	changed chan struct{}
}

func newFakeMessenger() *fakeMessenger {
	return &fakeMessenger{msgs: map[int]*sentMessage{}, changed: make(chan struct{}, 1000)}
}

func (f *fakeMessenger) Send(_ context.Context, chat int64, text string, kb Keyboard) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.msgs[f.nextID] = &sentMessage{chat: chat, id: f.nextID, text: text, kb: kb}
	f.changed <- struct{}{}
	return f.nextID, nil
}

func (f *fakeMessenger) Edit(_ context.Context, chat int64, id int, text string, kb Keyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.msgs[id]
	if m == nil || m.chat != chat {
		return fmt.Errorf("no message %d in chat %d", id, chat)
	}
	m.text, m.kb = text, kb
	f.changed <- struct{}{}
	return nil
}

func (f *fakeMessenger) Answer(context.Context, string, string) error { return nil }

func (f *fakeMessenger) find(chat int64, substr string) *sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := 1; id <= f.nextID; id++ {
		if m := f.msgs[id]; m != nil && m.chat == chat && strings.Contains(m.text, substr) {
			c := *m
			return &c
		}
	}
	return nil
}

func (f *fakeMessenger) waitFor(t *testing.T, chat int64, substr string) *sentMessage {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if m := f.find(chat, substr); m != nil {
			return m
		}
		select {
		case <-f.changed:
		case <-deadline:
			t.Fatalf("chat %d never got a message containing %q", chat, substr)
		}
	}
}

func buttonData(m *sentMessage, prefix string) string {
	for _, row := range m.kb {
		for _, b := range row {
			if strings.HasPrefix(b.Data, prefix) {
				return b.Data
			}
		}
	}
	return ""
}

const (
	adminID    = int64(1000)
	friendID   = int64(2000)
	strangerID = int64(3000)
)

func newTestBot(t *testing.T) (*Bot, *fakeMessenger, *fakePipeline) {
	t.Helper()
	p := newFakePipeline()
	m, cache := newTestManager(t, p, 3, 10)
	svc := &Service{resolver: &fakeResolver{ext: "80"}, manager: m, cache: cache,
		signer: Signer{key: []byte(strings.Repeat("k", 32)), base: "https://ipa.example.com"}}
	allow, err := OpenAllowlist(filepath.Join(t.TempDir(), "allow.json"))
	if err != nil {
		t.Fatal(err)
	}
	fm := newFakeMessenger()
	return NewBot(fm, svc, []int64{adminID}, allow, &fakeAuth{}, func() string { return "status" }), fm, p
}

func message(from int64, text string) tgUpdate {
	return tgUpdate{Message: &tgMessage{From: tgUser{ID: from, FirstName: fmt.Sprint("U", from), Username: fmt.Sprint("u", from)},
		Chat: tgChat{ID: from, Type: "private"}, Text: text}}
}

func click(from int64, m *sentMessage, data string) tgUpdate {
	return tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: from, FirstName: fmt.Sprint("U", from)},
		Message: &tgMessage{MessageID: m.id, Chat: tgChat{ID: m.chat, Type: "private"}, Text: m.text}, Data: data}}
}

func TestBotStrangerRequestsAccessAndAdminApproves(t *testing.T) {
	b, fm, _ := newTestBot(t)
	ctx := context.Background()

	b.Handle(ctx, message(strangerID, "414478124"))
	fm.waitFor(t, strangerID, "don't have access yet")
	req := fm.waitFor(t, adminID, "Access request")
	if !strings.Contains(req.text, "3000") {
		t.Fatalf("access request lacks the user ID: %s", req.text)
	}
	// A second message does not spam the admin.
	b.Handle(ctx, message(strangerID, "again"))
	count := 0
	for _, m := range fm.msgs {
		if m.chat == adminID && strings.Contains(m.text, "Access request") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("admin got %d access requests, want 1", count)
	}

	// Only an admin can approve.
	b.Handle(ctx, click(friendID, req, buttonData(req, callbackAllow)))
	if b.authorized(strangerID) {
		t.Fatal("non-admin approved a user")
	}
	b.Handle(ctx, click(adminID, req, buttonData(req, callbackAllow)))
	if !b.authorized(strangerID) {
		t.Fatal("stranger should be allowed after approval")
	}
	fm.waitFor(t, strangerID, "gave you access")

	b.Handle(ctx, message(adminID, "/revoke 3000"))
	if b.authorized(strangerID) {
		t.Fatal("revoke did not remove the user")
	}
	// Non-admins cannot use admin commands.
	b.Handle(ctx, message(strangerID, "/allow 4000"))
	if b.allow.Has(4000) {
		t.Fatal("stranger used /allow")
	}
}

func TestBotPreviewConfirmProgressAndAdminNotices(t *testing.T) {
	b, fm, p := newTestBot(t)
	ctx := context.Background()
	b.Handle(ctx, message(adminID, "/allow 2000 family"))
	if !b.authorized(friendID) {
		t.Fatal("/allow failed")
	}

	// Preview first: nothing is queued until the user confirms.
	b.Handle(ctx, message(friendID, "https://apps.apple.com/us/app/example/id12345"))
	preview := fm.waitFor(t, friendID, "Is this the right app")
	if !strings.Contains(preview.text, "8.0.80") || !strings.Contains(preview.text, "12345") {
		t.Fatalf("preview: %s", preview.text)
	}
	if s := b.svc.manager.Status(); s.Running+s.Queued != 0 {
		t.Fatal("task created before confirmation")
	}
	// Someone else cannot confirm the friend's preview.
	b.Handle(ctx, click(adminID, preview, buttonData(preview, callbackOK)))
	if s := b.svc.manager.Status(); s.Running+s.Queued != 0 {
		t.Fatal("another user confirmed the preview")
	}

	go b.Handle(ctx, click(friendID, preview, buttonData(preview, callbackOK)))
	fm.waitFor(t, adminID, "Notice · New request")
	<-p.started
	progress := fm.waitFor(t, friendID, "Downloading")
	if buttonData(progress, callbackCancel) == "" {
		t.Fatal("progress message has no cancel button")
	}
	close(p.gate("12345:80"))
	ready := fm.waitFor(t, friendID, "is ready")
	if len(ready.kb) == 0 || !strings.HasPrefix(ready.kb[0][0].URL, "https://ipa.example.com/d/") {
		t.Fatalf("ready message lacks download button: %+v", ready.kb)
	}
	done := fm.waitFor(t, adminID, "Notice · Task finished")
	if !strings.Contains(done.text, "u2000") {
		t.Fatalf("notice does not name the user: %s", done.text)
	}

	// Second request for the same build: cache hit, admin told so.
	b.Handle(ctx, message(friendID, "12345"))
	var again *sentMessage
	deadline := time.After(5 * time.Second)
	for again == nil {
		fm.mu.Lock()
		for id := fm.nextID; id > preview.id; id-- {
			if m := fm.msgs[id]; m.chat == friendID && strings.Contains(m.text, "is cached") {
				c := *m
				again = &c
				break
			}
		}
		fm.mu.Unlock()
		if again == nil {
			select {
			case <-fm.changed:
			case <-deadline:
				t.Fatal("no cached preview")
			}
		}
	}
	b.Handle(ctx, click(friendID, again, buttonData(again, callbackOK)))
	fm.waitFor(t, adminID, "Cache hit")
}

func TestBotAdminOwnRequestsSendNoNotices(t *testing.T) {
	b, fm, p := newTestBot(t)
	ctx := context.Background()
	b.Handle(ctx, message(adminID, "12345"))
	preview := fm.waitFor(t, adminID, "Is this the right app")
	go b.Handle(ctx, click(adminID, preview, buttonData(preview, callbackOK)))
	<-p.started
	close(p.gate("12345:80"))
	fm.waitFor(t, adminID, "is ready")
	if fm.find(adminID, "Notice") != nil {
		t.Fatal("admin was notified about their own request")
	}
}

func TestBotCancelOnlyOwnTask(t *testing.T) {
	b, fm, p := newTestBot(t)
	ctx := context.Background()
	b.allow.Allow(AllowedUser{ID: friendID})
	b.allow.Allow(AllowedUser{ID: strangerID})
	b.Handle(ctx, message(friendID, "12345"))
	preview := fm.waitFor(t, friendID, "Is this the right app")
	go b.Handle(ctx, click(friendID, preview, buttonData(preview, callbackOK)))
	<-p.started
	progress := fm.waitFor(t, friendID, "Downloading")
	data := buttonData(progress, callbackCancel)

	b.Handle(ctx, click(strangerID, progress, data))
	if v, _, _ := b.svc.manager.Get(strings.TrimPrefix(data, callbackCancel)); v.Status.Terminal() {
		t.Fatal("another user canceled the task")
	}
	b.Handle(ctx, click(friendID, progress, data))
	fm.waitFor(t, friendID, "Task canceled")
	fm.waitFor(t, adminID, "Notice · Task canceled")
}

func TestBotPreviewExpires(t *testing.T) {
	b, fm, _ := newTestBot(t)
	ctx := context.Background()
	b.Handle(ctx, message(adminID, "12345"))
	preview := fm.waitFor(t, adminID, "Is this the right app")
	b.now = func() time.Time { return time.Now().Add(previewTTL + time.Minute) }
	b.Handle(ctx, click(adminID, preview, buttonData(preview, callbackOK)))
	fm.waitFor(t, adminID, "expired")
	if s := b.svc.manager.Status(); s.Running+s.Queued != 0 {
		t.Fatal("expired preview created a task")
	}
}

func TestParseQuery(t *testing.T) {
	cases := []struct {
		in     string
		bundle string
		id     int64
		ok     bool
	}{
		{"414478124", "", 414478124, true},
		{"id414478124", "", 414478124, true},
		{"https://apps.apple.com/cn/app/%E5%BE%AE%E4%BF%A1/id414478124?l=en", "", 414478124, true},
		{"com.tencent.xin", "com.tencent.xin", 0, true},
		{"hello", "", 0, false},
		{"12", "", 0, false},
		{"com.foo/../bar", "", 0, false},
	}
	for _, c := range cases {
		bundle, id, ok := parseQuery(c.in)
		if bundle != c.bundle || id != c.id || ok != c.ok {
			t.Errorf("parseQuery(%q) = %q %d %v", c.in, bundle, id, ok)
		}
	}
	if cmd, args, ok := parseCommand("/allow@my_bot 123 note"); !ok || cmd != "allow" || args != "123 note" {
		t.Errorf("parseCommand: %q %q %v", cmd, args, ok)
	}
}

func TestRenderingEscapesHTML(t *testing.T) {
	info := AppInfo{Target: Target{AppID: 1, Name: "<b>Evil & Co</b>", BundleID: "a.b"}, Developer: "x<y"}
	out := renderPreview(info, false)
	if strings.Contains(out, "<b>Evil") || !strings.Contains(out, "&lt;b&gt;Evil &amp; Co") {
		t.Fatalf("not escaped: %s", out)
	}
	if got := formatBytes(734003200); got != "700.0 MB" {
		t.Errorf("formatBytes = %s", got)
	}
	if got := formatDuration(76 * time.Second); got != "1m 16s" {
		t.Errorf("formatDuration = %s", got)
	}
}

type fakeAuth struct {
	mu        sync.Mutex
	needsCode bool
	starts    int
	codes     []string
	searches  []string
}

func (f *fakeAuth) StartLogin() (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	return "me@example.com", f.needsCode, nil
}

func (f *fakeAuth) AccountSummary() AccountSummary {
	return AccountSummary{Email: "me@example.com", Name: "Me", SignedIn: true, AutoLogin: true, MaxFailures: 3}
}

func (f *fakeAuth) Search(_ context.Context, term string, limit int) ([]AppInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, term)
	return []AppInfo{
		{Target: Target{AppID: 12345, BundleID: "com.example.one", Name: "Example <One>", Version: "1.0"}},
		{Target: Target{AppID: 67890, BundleID: "com.example.two", Name: "Example Two", Version: "2.0"}, Price: 2.99},
	}, nil
}

func (f *fakeAuth) FinishLogin(code string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = append(f.codes, code)
	return "me@example.com", nil
}

func TestBotLoginIsAdminOnly(t *testing.T) {
	b, fm, _ := newTestBot(t)
	auth := b.auth.(*fakeAuth)
	ctx := context.Background()
	b.allow.Allow(AllowedUser{ID: friendID})

	b.Handle(ctx, message(friendID, "/login"))
	fm.waitFor(t, friendID, "Only admins")
	b.Handle(ctx, message(strangerID, "/login"))
	fm.waitFor(t, strangerID, "don't have access yet")
	if auth.starts != 0 {
		t.Fatal("non-admin started a login")
	}

	b.Handle(ctx, message(adminID, "/login"))
	fm.waitFor(t, adminID, "Signed in as")
	if auth.starts != 1 {
		t.Fatalf("starts = %d", auth.starts)
	}
}

func TestBotLoginTwoFactorCodeOnlyFromInitiatingAdmin(t *testing.T) {
	b, fm, _ := newTestBot(t)
	auth := b.auth.(*fakeAuth)
	auth.needsCode = true
	ctx := context.Background()
	b.allow.Allow(AllowedUser{ID: friendID})

	b.Handle(ctx, message(adminID, "/login"))
	fm.waitFor(t, adminID, "6-digit code")

	// An allowed non-admin sending six digits gets an app lookup, not a login.
	b.Handle(ctx, message(friendID, "123456"))
	fm.waitFor(t, friendID, "Is this the right app")
	if len(auth.codes) != 0 {
		t.Fatal("non-admin message was used as a 2FA code")
	}

	b.Handle(ctx, message(adminID, "654 321"))
	fm.waitFor(t, adminID, "Signed in as")
	if len(auth.codes) != 1 || auth.codes[0] != "654321" {
		t.Fatalf("codes = %v", auth.codes)
	}
	// The wait is consumed: the next six digits are an app ID again.
	b.Handle(ctx, message(adminID, "111111"))
	if len(auth.codes) != 1 {
		t.Fatal("code accepted without a pending login")
	}
}

func TestBotLoginCodeExpires(t *testing.T) {
	b, fm, _ := newTestBot(t)
	auth := b.auth.(*fakeAuth)
	auth.needsCode = true
	ctx := context.Background()
	b.Handle(ctx, message(adminID, "/login"))
	fm.waitFor(t, adminID, "6-digit code")
	b.now = func() time.Time { return time.Now().Add(loginCodeTTL + time.Minute) }
	b.Handle(ctx, message(adminID, "/login 123456"))
	fm.waitFor(t, adminID, "No sign-in is waiting")
	if len(auth.codes) != 0 {
		t.Fatal("expired code accepted")
	}
}

func TestHealthNotifiesOnlyOnChange(t *testing.T) {
	b, fm, _ := newTestBot(t)
	var fail error = &TaskError{Code: "auth_required", Message: "token expired"}
	var mu sync.Mutex
	h := NewHealth(func(context.Context) error { mu.Lock(); defer mu.Unlock(); return fail }, time.Hour)
	h.SetOnChange(b.HealthChanged)
	ctx := context.Background()

	h.CheckNow(ctx)
	n := fm.waitFor(t, adminID, "App Store connection failing")
	if !strings.Contains(n.text, "/login") {
		t.Fatalf("auth failure notice lacks the /login hint: %s", n.text)
	}
	h.CheckNow(ctx)
	count := 0
	for _, m := range fm.msgs {
		if strings.Contains(m.text, "App Store connection failing") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d failure notices, want 1", count)
	}
	mu.Lock()
	fail = nil
	mu.Unlock()
	if st := h.CheckNow(ctx); !st.OK {
		t.Fatal("expected healthy")
	}
	fm.waitFor(t, adminID, "App Store connection restored")
}

func TestBotPreviewNeverAcquiresLicense(t *testing.T) {
	b, fm, p := newTestBot(t)
	res := b.svc.resolver.(*fakeResolver)
	res.unowned = true
	ctx := context.Background()

	b.Handle(ctx, message(adminID, "12345"))
	preview := fm.waitFor(t, adminID, "doesn't have it yet")
	if len(res.purchases) != 1 || res.purchases[0] {
		t.Fatalf("preview resolved with purchase flags %v, want [false]", res.purchases)
	}

	// Canceling leaves the account untouched.
	b.Handle(ctx, click(adminID, preview, buttonData(preview, callbackNo)))
	if len(res.purchases) != 1 {
		t.Fatalf("cancel resolved again: %v", res.purchases)
	}

	// Confirming acquires the license, then downloads the exact build.
	b.Handle(ctx, message(adminID, "12345"))
	var again *sentMessage
	for again == nil {
		<-fm.changed
		fm.mu.Lock()
		if m := fm.msgs[fm.nextID]; m.chat == adminID && strings.Contains(m.text, "doesn't have it yet") && m.id != preview.id {
			c := *m
			again = &c
		}
		fm.mu.Unlock()
	}
	go b.Handle(ctx, click(adminID, again, buttonData(again, callbackOK)))
	if key := <-p.started; key != "12345:80" {
		t.Fatalf("downloaded %s", key)
	}
	res.mu.Lock()
	flags := append([]bool(nil), res.purchases...)
	res.mu.Unlock()
	if len(flags) != 3 || flags[1] || !flags[2] {
		t.Fatalf("purchase flags %v, want [false false true]", flags)
	}
	close(p.gate("12345:80"))
	fm.waitFor(t, adminID, "is ready")
}

func TestBotSearchListsResultsThatOpenPreview(t *testing.T) {
	b, fm, _ := newTestBot(t)
	auth := b.auth.(*fakeAuth)
	res := b.svc.resolver.(*fakeResolver)
	ctx := context.Background()
	b.allow.Allow(AllowedUser{ID: friendID})

	b.Handle(ctx, message(strangerID, "/search wechat"))
	fm.waitFor(t, strangerID, "don't have access yet")
	if len(auth.searches) != 0 {
		t.Fatal("a stranger could search")
	}

	b.Handle(ctx, message(friendID, "/search"))
	fm.waitFor(t, friendID, "Usage: <code>/search")

	b.Handle(ctx, message(friendID, "/search wechat"))
	list := fm.waitFor(t, friendID, "Results for")
	if !strings.Contains(list.text, "Example &lt;One&gt;") || !strings.Contains(list.text, "2.99") {
		t.Fatalf("results: %s", list.text)
	}
	data := buttonData(list, callbackPreview)
	if data != callbackPreview+"12345" || len(list.kb) != 2 {
		t.Fatalf("buttons: %+v", list.kb)
	}

	b.Handle(ctx, click(friendID, list, data))
	fm.waitFor(t, friendID, "Is this the right app")
	if len(res.purchases) != 1 || res.purchases[0] {
		t.Fatalf("search preview resolved with purchase flags %v", res.purchases)
	}
	if s := b.svc.manager.Status(); s.Running+s.Queued != 0 {
		t.Fatal("search created a task")
	}

	// Plain text that is not an ID points to /search instead of searching.
	b.Handle(ctx, message(friendID, "wechat"))
	fm.waitFor(t, friendID, "/search wechat")
	if len(auth.searches) != 1 {
		t.Fatal("plain text should not search")
	}
}

func TestBotHealthIsAdminOnly(t *testing.T) {
	b, fm, _ := newTestBot(t)
	ctx := context.Background()
	b.allow.Allow(AllowedUser{ID: friendID})
	checks := 0
	b.svc.health = NewHealth(func(context.Context) error { checks++; return nil }, time.Hour)

	b.Handle(ctx, message(friendID, "/health"))
	fm.waitFor(t, friendID, "Only admins")
	if checks != 0 {
		t.Fatal("non-admin triggered a health check")
	}

	b.Handle(ctx, message(adminID, "/health"))
	m := fm.waitFor(t, adminID, "App Store: ✅ OK")
	for _, want := range []string{"Me · me@example.com", "Session: ✅ signed in", "Auto sign-in: ✅ enabled"} {
		if !strings.Contains(m.text, want) {
			t.Fatalf("health report lacks %q:\n%s", want, m.text)
		}
	}
	if checks != 1 {
		t.Fatalf("checks = %d, want a live check", checks)
	}
}

func TestBotClearIsAdminOnlyAndConfirmed(t *testing.T) {
	b, fm, _ := newTestBot(t)
	ctx := context.Background()
	b.allow.Allow(AllowedUser{ID: friendID})
	for _, key := range []string{"1:1", "2:1"} {
		src := filepath.Join(t.TempDir(), "a.ipa")
		os.WriteFile(src, []byte("ipa"), 0o600)
		if _, err := b.svc.cache.Add(Artifact{Key: key, Size: 3}, src); err != nil {
			t.Fatal(err)
		}
	}

	b.Handle(ctx, message(friendID, "/clear"))
	fm.waitFor(t, friendID, "Only admins")

	b.Handle(ctx, message(adminID, "/clear"))
	ask := fm.waitFor(t, adminID, "Delete all <b>2</b>")
	if b.svc.cache.Stats().Artifacts != 2 {
		t.Fatal("cleared before confirmation")
	}
	// A non-admin pressing the button does nothing.
	b.Handle(ctx, click(friendID, ask, callbackClear+"yes"))
	if b.svc.cache.Stats().Artifacts != 2 {
		t.Fatal("non-admin cleared the cache")
	}
	b.Handle(ctx, click(adminID, ask, callbackClear+"no"))
	if b.svc.cache.Stats().Artifacts != 2 {
		t.Fatal("cancel cleared the cache")
	}
	b.Handle(ctx, click(adminID, ask, callbackClear+"yes"))
	fm.waitFor(t, adminID, "Deleted 2 cached IPA(s)")
	if st := b.svc.cache.Stats(); st.Artifacts != 0 || st.Bytes != 0 {
		t.Fatalf("stats after clear: %+v", st)
	}
	entries, _ := os.ReadDir(b.svc.cache.dir)
	if len(entries) != 0 {
		t.Fatalf("files left in the cache directory: %d", len(entries))
	}
	b.Handle(ctx, message(adminID, "/clear"))
	fm.waitFor(t, adminID, "already empty")
}
