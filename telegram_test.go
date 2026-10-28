package main

import (
	"context"
	"fmt"
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
	return NewBot(fm, svc, []int64{adminID}, allow, func() string { return "status" }), fm, p
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
	fm.waitFor(t, strangerID, "还没有使用权限")
	req := fm.waitFor(t, adminID, "访问申请")
	if !strings.Contains(req.text, "3000") {
		t.Fatalf("access request lacks the user ID: %s", req.text)
	}
	// A second message does not spam the admin.
	b.Handle(ctx, message(strangerID, "again"))
	count := 0
	for _, m := range fm.msgs {
		if m.chat == adminID && strings.Contains(m.text, "访问申请") {
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
	fm.waitFor(t, strangerID, "已为你开通")

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
	preview := fm.waitFor(t, friendID, "是这个 App 吗")
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
	fm.waitFor(t, adminID, "通知 · 新请求")
	<-p.started
	progress := fm.waitFor(t, friendID, "下载中")
	if buttonData(progress, callbackCancel) == "" {
		t.Fatal("progress message has no cancel button")
	}
	close(p.gate("12345:80"))
	ready := fm.waitFor(t, friendID, "已就绪")
	if len(ready.kb) == 0 || !strings.HasPrefix(ready.kb[0][0].URL, "https://ipa.example.com/d/") {
		t.Fatalf("ready message lacks download button: %+v", ready.kb)
	}
	done := fm.waitFor(t, adminID, "通知 · 任务完成")
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
			if m := fm.msgs[id]; m.chat == friendID && strings.Contains(m.text, "已有缓存") {
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
	fm.waitFor(t, adminID, "命中缓存")
}

func TestBotAdminOwnRequestsSendNoNotices(t *testing.T) {
	b, fm, p := newTestBot(t)
	ctx := context.Background()
	b.Handle(ctx, message(adminID, "12345"))
	preview := fm.waitFor(t, adminID, "是这个 App 吗")
	go b.Handle(ctx, click(adminID, preview, buttonData(preview, callbackOK)))
	<-p.started
	close(p.gate("12345:80"))
	fm.waitFor(t, adminID, "已就绪")
	if fm.find(adminID, "通知") != nil {
		t.Fatal("admin was notified about their own request")
	}
}

func TestBotCancelOnlyOwnTask(t *testing.T) {
	b, fm, p := newTestBot(t)
	ctx := context.Background()
	b.allow.Allow(AllowedUser{ID: friendID})
	b.allow.Allow(AllowedUser{ID: strangerID})
	b.Handle(ctx, message(friendID, "12345"))
	preview := fm.waitFor(t, friendID, "是这个 App 吗")
	go b.Handle(ctx, click(friendID, preview, buttonData(preview, callbackOK)))
	<-p.started
	progress := fm.waitFor(t, friendID, "下载中")
	data := buttonData(progress, callbackCancel)

	b.Handle(ctx, click(strangerID, progress, data))
	if v, _, _ := b.svc.manager.Get(strings.TrimPrefix(data, callbackCancel)); v.Status.Terminal() {
		t.Fatal("another user canceled the task")
	}
	b.Handle(ctx, click(friendID, progress, data))
	fm.waitFor(t, friendID, "任务已取消")
	fm.waitFor(t, adminID, "通知 · 任务取消")
}

func TestBotPreviewExpires(t *testing.T) {
	b, fm, _ := newTestBot(t)
	ctx := context.Background()
	b.Handle(ctx, message(adminID, "12345"))
	preview := fm.waitFor(t, adminID, "是这个 App 吗")
	b.now = func() time.Time { return time.Now().Add(previewTTL + time.Minute) }
	b.Handle(ctx, click(adminID, preview, buttonData(preview, callbackOK)))
	fm.waitFor(t, adminID, "已过期")
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
	if got := formatDuration(76 * time.Second); got != "1分16秒" {
		t.Errorf("formatDuration = %s", got)
	}
}
