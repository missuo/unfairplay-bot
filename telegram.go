package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	previewTTL        = 10 * time.Minute
	progressInterval  = 3 * time.Second
	accessRequestGap  = 6 * time.Hour
	callbackOK        = "ok:"
	callbackNo        = "no:"
	callbackCancel    = "cx:"
	callbackAllow     = "al:"
	callbackIgnore    = "ig:"
	callbackRevoke    = "rv:"
	noticeHeaderEmoji = "🔔"
	loginCodeTTL      = 10 * time.Minute
)

// Authenticator signs the App Store account in again (admin-only /login).
type Authenticator interface {
	StartLogin() (email string, needsCode bool, err error)
	FinishLogin(code string) (email string, err error)
}

type pendingRequest struct {
	user    tgUser
	chatID  int64
	info    AppInfo
	created time.Time
}

// Bot is the Telegram front end. Users talk to it in private chats; admins
// additionally receive notices about everyone else's activity.
type Bot struct {
	api       Messenger
	svc       *Service
	admins    map[int64]bool
	allow     *Allowlist
	statusFn  func() string
	now       func() time.Time
	mu        sync.Mutex
	pending   map[string]*pendingRequest
	watchers  map[string]map[int64]bool // task ID -> users following it
	requested map[int64]time.Time       // last access request per stranger
	auth      Authenticator
	loginWait map[int64]time.Time // admin -> when Apple sent a 2FA code
}

func NewBot(api Messenger, svc *Service, admins []int64, allow *Allowlist, auth Authenticator, statusFn func() string) *Bot {
	b := &Bot{
		api: api, svc: svc, admins: map[int64]bool{}, allow: allow, auth: auth, statusFn: statusFn, now: time.Now,
		pending: map[string]*pendingRequest{}, watchers: map[string]map[int64]bool{}, requested: map[int64]time.Time{},
		loginWait: map[int64]time.Time{},
	}
	for _, id := range admins {
		b.admins[id] = true
	}
	return b
}

var userCommands = []botCommand{
	{Command: "start", Description: "How to use this bot"},
	{Command: "help", Description: "How to use this bot"},
}

var adminCommands = append(append([]botCommand{}, userCommands...),
	botCommand{Command: "status", Description: "Service status and active tasks"},
	botCommand{Command: "users", Description: "Allowed users"},
	botCommand{Command: "allow", Description: "Allow a user: /allow <ID> [note]"},
	botCommand{Command: "revoke", Description: "Remove a user: /revoke <ID>"},
	botCommand{Command: "login", Description: "Sign in to the Apple ID again"},
)

// Run long-polls Telegram until ctx is canceled.
func (b *Bot) Run(ctx context.Context, client *tgClient) {
	if err := client.setCommands(ctx, userCommands, 0); err != nil {
		slog.Warn("telegram: set commands", "err", err)
	}
	for id := range b.admins {
		if err := client.setCommands(ctx, adminCommands, id); err != nil {
			slog.Warn("telegram: set admin commands (has the admin started the bot?)", "admin", id, "err", err)
		}
	}
	offset := 0
	for ctx.Err() == nil {
		updates, err := client.updates(ctx, offset)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("telegram: getUpdates", "err", err)
				select {
				case <-time.After(3 * time.Second):
				case <-ctx.Done():
				}
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			go b.Handle(ctx, u)
		}
	}
}

func (b *Bot) authorized(id int64) bool { return b.admins[id] || b.allow.Has(id) }

func (b *Bot) Handle(ctx context.Context, u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("telegram: handler panic", "panic", r)
		}
	}()
	switch {
	case u.Message != nil:
		b.handleMessage(ctx, u.Message)
	case u.Callback != nil:
		b.handleCallback(ctx, u.Callback)
	}
}

func (b *Bot) send(ctx context.Context, chatID int64, text string, kb Keyboard) int {
	id, err := b.api.Send(ctx, chatID, text, kb)
	if err != nil {
		slog.Warn("telegram: send", "chat", chatID, "err", err)
	}
	return id
}

func (b *Bot) edit(ctx context.Context, chatID int64, msgID int, text string, kb Keyboard) {
	if err := b.api.Edit(ctx, chatID, msgID, text, kb); err != nil {
		slog.Warn("telegram: edit", "chat", chatID, "err", err)
	}
}

// notifyAdmins sends a notice to every admin except the acting user.
func (b *Bot) notifyAdmins(ctx context.Context, actor int64, text string, kb Keyboard) {
	for id := range b.admins {
		if id != actor {
			b.send(ctx, id, text, kb)
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, m *tgMessage) {
	if m.Chat.Type != "private" || m.From.ID == 0 {
		return
	}
	user := m.From
	if !b.authorized(user.ID) {
		b.handleStranger(ctx, m)
		return
	}
	b.allow.Refresh(user.ID, user.DisplayName(), user.Username)

	text := strings.TrimSpace(m.Text)
	// A 6-digit reply is a 2FA code only from the admin who started /login.
	if code, ok := parseAuthCode(text); ok && b.admins[user.ID] && b.takeLoginWait(user.ID) {
		b.finishLogin(ctx, m.Chat.ID, code)
		return
	}
	if cmd, args, ok := parseCommand(text); ok {
		b.handleCommand(ctx, m, cmd, args)
		return
	}
	bundleID, appID, ok := parseQuery(text)
	if !ok {
		b.send(ctx, m.Chat.ID, helpText(b.admins[user.ID]), nil)
		return
	}
	b.preview(ctx, m.Chat.ID, user, bundleID, appID)
}

func (b *Bot) handleStranger(ctx context.Context, m *tgMessage) {
	user := m.From
	b.send(ctx, m.Chat.ID, fmt.Sprintf("🔒 You don't have access yet.\n\nYour Telegram ID: <code>%d</code>\nThe admin has been notified. You'll get a message once you're approved.", user.ID), nil)
	b.mu.Lock()
	last, seen := b.requested[user.ID]
	if seen && b.now().Sub(last) < accessRequestGap {
		b.mu.Unlock()
		return
	}
	b.requested[user.ID] = b.now()
	b.mu.Unlock()
	id := strconv.FormatInt(user.ID, 10)
	b.notifyAdmins(ctx, 0, notice("Access request", userLine(user), "💬 "+html.EscapeString(truncate(m.Text, 100))),
		Keyboard{{{Text: "✅ Allow", Data: callbackAllow + id}, {Text: "🚫 Ignore", Data: callbackIgnore + id}}})
}

func (b *Bot) handleCommand(ctx context.Context, m *tgMessage, cmd, args string) {
	isAdmin := b.admins[m.From.ID]
	switch {
	case cmd == "start" || cmd == "help":
		b.send(ctx, m.Chat.ID, helpText(isAdmin), nil)
	case !isAdmin:
		b.send(ctx, m.Chat.ID, "Only admins can use this command.", nil)
	case cmd == "status":
		b.send(ctx, m.Chat.ID, b.statusFn(), nil)
	case cmd == "users":
		b.sendUsers(ctx, m.Chat.ID)
	case cmd == "allow":
		fields := strings.Fields(args)
		id, err := strconv.ParseInt(firstOr(fields, ""), 10, 64)
		if err != nil || id <= 0 {
			b.send(ctx, m.Chat.ID, "Usage: <code>/allow &lt;Telegram ID&gt; [note]</code>", nil)
			return
		}
		note := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
		b.allowUser(ctx, m.Chat.ID, m.From.ID, AllowedUser{ID: id, Note: note})
	case cmd == "login":
		if code, ok := parseAuthCode(args); ok {
			if b.takeLoginWait(m.From.ID) {
				b.finishLogin(ctx, m.Chat.ID, code)
			} else {
				b.send(ctx, m.Chat.ID, "No sign-in is waiting for a code. Send /login first.", nil)
			}
			return
		}
		b.startLogin(ctx, m.Chat.ID, m.From.ID)
	case cmd == "revoke":
		id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
		if err != nil {
			b.send(ctx, m.Chat.ID, "Usage: <code>/revoke &lt;Telegram ID&gt;</code>", nil)
			return
		}
		b.revokeUser(ctx, m.Chat.ID, id)
	default:
		b.send(ctx, m.Chat.ID, helpText(true), nil)
	}
}

func (b *Bot) allowUser(ctx context.Context, chatID, adminID int64, u AllowedUser) {
	if b.admins[u.ID] {
		b.send(ctx, chatID, "That user is an admin and already has access.", nil)
		return
	}
	u.AddedBy, u.AddedAt = adminID, b.now()
	added, err := b.allow.Allow(u)
	switch {
	case err != nil:
		b.send(ctx, chatID, "Couldn't save the allowlist: "+html.EscapeString(err.Error()), nil)
	case !added:
		b.send(ctx, chatID, fmt.Sprintf("<code>%d</code> is already allowed.", u.ID), nil)
	default:
		b.send(ctx, chatID, fmt.Sprintf("✅ Allowed %s.", allowedLabel(u)), nil)
		b.send(ctx, u.ID, "✅ An admin gave you access.\n\n"+helpText(false), nil)
	}
}

func (b *Bot) revokeUser(ctx context.Context, chatID, id int64) {
	u, removed, err := b.allow.Revoke(id)
	switch {
	case err != nil:
		b.send(ctx, chatID, "Couldn't save the allowlist: "+html.EscapeString(err.Error()), nil)
	case !removed:
		b.send(ctx, chatID, fmt.Sprintf("<code>%d</code> isn't on the allowlist.", id), nil)
	default:
		b.send(ctx, chatID, fmt.Sprintf("Removed %s from the allowlist.", allowedLabel(u)), nil)
	}
}

func (b *Bot) sendUsers(ctx context.Context, chatID int64) {
	users := b.allow.List()
	if len(users) == 0 {
		b.send(ctx, chatID, "The allowlist is empty. Add someone with <code>/allow &lt;ID&gt;</code>, or tap Allow on an access request.", nil)
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "👥 <b>Allowed users</b> (%d)\n", len(users))
	var kb Keyboard
	for _, u := range users {
		fmt.Fprintf(&sb, "\n• %s · added %s", allowedLabel(u), u.AddedAt.Local().Format("2006-01-02"))
		if u.Note != "" {
			fmt.Fprintf(&sb, "\n  Note: %s", html.EscapeString(u.Note))
		}
		kb = append(kb, []Button{{Text: "Remove " + truncate(firstNonEmpty(u.Name, u.Username, strconv.FormatInt(u.ID, 10)), 24), Data: callbackRevoke + strconv.FormatInt(u.ID, 10)}})
	}
	b.send(ctx, chatID, sb.String(), kb)
}

// preview resolves the app and asks the user to confirm the exact build.
func (b *Bot) preview(ctx context.Context, chatID int64, user tgUser, bundleID string, appID int64) {
	msgID := b.send(ctx, chatID, "🔍 Looking up the App Store…", nil)
	info, err := b.svc.Resolve(ctx, bundleID, appID)
	if err != nil {
		te := classify(err)
		b.edit(ctx, chatID, msgID, "❌ Lookup failed: "+friendlyError(te), nil)
		return
	}
	pid := shortID()
	b.mu.Lock()
	for id, p := range b.pending {
		if b.now().Sub(p.created) > previewTTL {
			delete(b.pending, id)
		}
	}
	b.pending[pid] = &pendingRequest{user: user, chatID: chatID, info: info, created: b.now()}
	b.mu.Unlock()
	cached := b.svc.IsCached(info.Target)
	action := "✅ Download & decrypt"
	if cached {
		action = "✅ Get download link"
	}
	b.edit(ctx, chatID, msgID, renderPreview(info, cached),
		Keyboard{{{Text: action, Data: callbackOK + pid}, {Text: "❌ Cancel", Data: callbackNo + pid}}})
}

func (b *Bot) handleCallback(ctx context.Context, cb *tgCallback) {
	answer := ""
	defer func() { _ = b.api.Answer(ctx, cb.ID, answer) }()
	if cb.Message == nil || !b.authorized(cb.From.ID) {
		answer = "Not allowed"
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	kind, arg, _ := strings.Cut(cb.Data, ":")
	kind += ":"
	isAdmin := b.admins[cb.From.ID]

	switch kind {
	case callbackOK, callbackNo:
		b.mu.Lock()
		p := b.pending[arg]
		if p != nil && p.user.ID == cb.From.ID {
			delete(b.pending, arg)
		}
		b.mu.Unlock()
		if p == nil || p.user.ID != cb.From.ID || b.now().Sub(p.created) > previewTTL {
			answer = "Expired, please send it again"
			b.edit(ctx, chatID, msgID, "⌛ This confirmation expired. Send the bundle ID or App ID again.", nil)
			return
		}
		if kind == callbackNo {
			b.edit(ctx, chatID, msgID, renderPreview(p.info, false)+"\n\n🚫 Canceled", nil)
			return
		}
		b.start(ctx, p, msgID)

	case callbackCancel:
		b.mu.Lock()
		following := b.watchers[arg][cb.From.ID]
		b.mu.Unlock()
		if !following && !isAdmin {
			answer = "You can only cancel your own tasks"
			return
		}
		if _, ok := b.svc.manager.Cancel(arg); !ok {
			answer = "Task not found"
			return
		}
		answer = "Canceling…"

	case callbackAllow, callbackIgnore, callbackRevoke:
		if !isAdmin {
			answer = "Admins only"
			return
		}
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return
		}
		switch kind {
		case callbackAllow:
			b.allowUser(ctx, chatID, cb.From.ID, AllowedUser{ID: id})
			b.edit(ctx, chatID, msgID, html.EscapeString(cb.Message.Text)+"\n\n✅ Allowed", nil)
		case callbackIgnore:
			b.edit(ctx, chatID, msgID, html.EscapeString(cb.Message.Text)+"\n\n🚫 Ignored", nil)
		case callbackRevoke:
			b.revokeUser(ctx, chatID, id)
		}
	}
}

// start submits a confirmed build and follows it until it finishes.
func (b *Bot) start(ctx context.Context, p *pendingRequest, msgID int) {
	info, user := p.info, p.user
	out, err := b.svc.Request(info.Target)
	if err != nil {
		code := "internal"
		switch {
		case errors.Is(err, ErrQueueFull):
			code = "queue_full"
		case errors.Is(err, ErrDraining):
			code = "draining"
		}
		b.edit(ctx, p.chatID, msgID, renderPreview(info, false)+"\n\n❌ "+friendlyError(&TaskError{Code: code, Message: err.Error()}), nil)
		return
	}
	admin := b.admins[user.ID]

	if out.Cached && out.Artifact != nil {
		text, kb := renderReady(info, *out.Artifact, true)
		b.edit(ctx, p.chatID, msgID, text, kb)
		if !admin {
			b.notifyAdmins(ctx, user.ID, notice("New request", userLine(user), appLine(info), "📌 Cache hit, link sent right away"), nil)
		}
		return
	}

	task := out.Task
	if !admin {
		how := "Task created"
		if !out.Created {
			how = "Joined a task already in progress"
		}
		if task.Status == StageQueued {
			how += fmt.Sprintf(" (queued, #%d)", task.Position)
		}
		b.notifyAdmins(ctx, user.ID, notice("New request", userLine(user), appLine(info), "📌 "+how), nil)
	}
	b.mu.Lock()
	if b.watchers[task.ID] == nil {
		b.watchers[task.ID] = map[int64]bool{}
	}
	b.watchers[task.ID][user.ID] = true
	b.mu.Unlock()
	b.follow(ctx, p.chatID, msgID, user, info, task.ID)
}

// follow keeps one message updated with the task's progress, editing at most
// every few seconds to stay inside Telegram's rate limits.
func (b *Bot) follow(ctx context.Context, chatID int64, msgID int, user tgUser, info AppInfo, taskID string) {
	defer func() {
		b.mu.Lock()
		delete(b.watchers[taskID], user.ID)
		if len(b.watchers[taskID]) == 0 {
			delete(b.watchers, taskID)
		}
		b.mu.Unlock()
	}()
	var last string
	var lastEdit time.Time
	for {
		v, changed, ok := b.svc.manager.Get(taskID)
		if !ok {
			return
		}
		v = b.svc.TaskView(v)
		terminal := v.Status.Terminal()
		text, kb := renderTask(info, v, b.now())
		if text != last {
			if wait := progressInterval - b.now().Sub(lastEdit); wait > 0 && !terminal {
				select {
				case <-time.After(wait):
					continue
				case <-ctx.Done():
					return
				}
			}
			b.edit(ctx, chatID, msgID, text, kb)
			last, lastEdit = text, b.now()
		}
		if terminal {
			b.finished(ctx, user, info, v)
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

func (b *Bot) finished(ctx context.Context, user tgUser, info AppInfo, v TaskView) {
	if b.admins[user.ID] {
		return
	}
	elapsed := ""
	if v.StartedAt != nil && v.FinishedAt != nil {
		elapsed = "⏱ Took " + formatDuration(v.FinishedAt.Sub(*v.StartedAt))
	}
	switch v.Status {
	case StageReady:
		size := ""
		if v.Artifact != nil {
			size = " · " + formatBytes(v.Artifact.Size)
		}
		b.notifyAdmins(ctx, user.ID, notice("Task finished", userLine(user), appLine(info), elapsed+size), nil)
	case StageFailed:
		b.notifyAdmins(ctx, user.ID, notice("Task failed", userLine(user), appLine(info),
			fmt.Sprintf("❗ <code>%s</code> %s", v.Error.Code, html.EscapeString(truncate(v.Error.Message, 300)))), nil)
	case StageCanceled:
		b.notifyAdmins(ctx, user.ID, notice("Task canceled", userLine(user), appLine(info)), nil)
	}
}

// ---- App Store session (admin only) ----

func (b *Bot) takeLoginWait(adminID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.loginWait[adminID]
	delete(b.loginWait, adminID)
	return ok && b.now().Sub(at) < loginCodeTTL
}

func (b *Bot) startLogin(ctx context.Context, chatID, adminID int64) {
	if !b.admins[adminID] { // also enforced by handleCommand
		return
	}
	msgID := b.send(ctx, chatID, "🔐 Signing in to the Apple ID again…", nil)
	email, needsCode, err := b.auth.StartLogin()
	switch {
	case err != nil:
		b.edit(ctx, chatID, msgID, "❌ Sign-in failed: "+friendlyLoginError(err), nil)
	case needsCode:
		b.mu.Lock()
		b.loginWait[adminID] = b.now()
		b.mu.Unlock()
		b.edit(ctx, chatID, msgID, fmt.Sprintf("📲 Apple sent a verification code to the trusted devices of <b>%s</b>.\n\nReply with the 6-digit code within 10 minutes.", html.EscapeString(email)), nil)
	default:
		b.edit(ctx, chatID, msgID, fmt.Sprintf("✅ Signed in as <b>%s</b>. Re-checking the App Store connection…", html.EscapeString(email)), nil)
		b.recheck(ctx, chatID)
	}
}

func (b *Bot) finishLogin(ctx context.Context, chatID int64, code string) {
	msgID := b.send(ctx, chatID, "🔐 Verifying…", nil)
	email, err := b.auth.FinishLogin(code)
	if err != nil {
		b.edit(ctx, chatID, msgID, "❌ Sign-in failed: "+friendlyLoginError(err)+"\n\nYou can send /login again.", nil)
		return
	}
	b.edit(ctx, chatID, msgID, fmt.Sprintf("✅ Signed in as <b>%s</b>. Re-checking the App Store connection…", html.EscapeString(email)), nil)
	b.recheck(ctx, chatID)
}

// recheck runs the health check now and reports the result to chatID; other
// admins hear about a recovery through the usual change notice.
func (b *Bot) recheck(ctx context.Context, chatID int64) {
	if h := b.svc.health; h != nil && h.check != nil {
		b.send(ctx, chatID, "🩺 App Store: "+healthLine(h.CheckNow(ctx)), nil)
	}
}

func friendlyLoginError(err error) string {
	if errors.Is(err, errNoCredentials) {
		return "No Apple ID credentials available. Set UNFAIRPLAY_BOT_APPLE_ID_EMAIL / UNFAIRPLAY_BOT_APPLE_ID_PASSWORD, or run <code>ipatool auth login</code> on the server."
	}
	return "<code>" + html.EscapeString(truncate(err.Error(), 300)) + "</code>"
}

// HealthChanged notifies admins when the App Store side breaks or recovers.
func (b *Bot) HealthChanged(_, cur HealthState) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cur.OK {
		b.notifyAdmins(ctx, 0, notice("App Store connection restored", "✅ Sign-in and downloads are working again"), nil)
		return
	}
	hint := "Apple may have changed its API. ipatool updates are picked up every 6 hours, and the server updates itself once a fix is released."
	if cur.Error.Code == "auth_required" {
		hint = "The Apple ID session is gone and automatic sign-in didn't work. Check the password in the environment, or send /login to retry."
	}
	b.notifyAdmins(ctx, 0, notice("App Store connection failing",
		fmt.Sprintf("❗ <code>%s</code> %s", cur.Error.Code, html.EscapeString(truncate(cur.Error.Message, 300))),
		"👉 "+hint), nil)
}

func healthLine(st HealthState) string {
	switch {
	case !st.Enabled:
		return "not configured (set UNFAIRPLAY_BOT_HEALTH_APP_ID)"
	case st.CheckedAt.IsZero():
		return "not checked yet"
	case st.OK:
		return "✅ OK (checked " + st.CheckedAt.Local().Format("Jan 2 15:04") + ")"
	}
	return fmt.Sprintf("❌ failing: <code>%s</code> (since %s)", st.Error.Code, st.Since.Local().Format("Jan 2 15:04"))
}

// ---- parsing ----

var (
	appStoreURLPattern = regexp.MustCompile(`apps\.apple\.com/.*?/id(\d+)`)
	appIDPattern       = regexp.MustCompile(`^(?:id)?(\d{5,12})$`)
	commandPattern     = regexp.MustCompile(`^/([a-z_]+)(?:@\w+)?(?:\s+(.*))?$`)
)

var authCodePattern = regexp.MustCompile(`^\d{6}$`)

func parseAuthCode(text string) (string, bool) {
	code := strings.ReplaceAll(strings.TrimSpace(text), " ", "")
	return code, authCodePattern.MatchString(code)
}

func parseCommand(text string) (string, string, bool) {
	m := commandPattern.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// parseQuery accepts an App ID, an App Store URL, or a bundle ID.
func parseQuery(text string) (string, int64, bool) {
	text = strings.TrimSpace(text)
	if m := appStoreURLPattern.FindStringSubmatch(text); m != nil {
		id, err := strconv.ParseInt(m[1], 10, 64)
		return "", id, err == nil
	}
	if m := appIDPattern.FindStringSubmatch(text); m != nil {
		id, err := strconv.ParseInt(m[1], 10, 64)
		return "", id, err == nil
	}
	if strings.Contains(text, ".") && bundleIDPattern.MatchString(text) {
		return text, 0, true
	}
	return "", 0, false
}

// ---- rendering ----

func helpText(admin bool) string {
	s := "📦 <b>unfairplay-bot</b>\n\n" +
		"Send any of these and I'll look the app up so you can confirm it:\n" +
		"• App ID: <code>414478124</code>\n" +
		"• Bundle ID: <code>com.tencent.xin</code>\n" +
		"• App Store link: <code>https://apps.apple.com/app/id414478124</code>\n\n" +
		"Once you confirm, I download the current version, decrypt it, and send you a download link.\n" +
		"If that version is already cached, you get the link right away."
	if admin {
		s += "\n\n<b>Admin commands</b>\n" +
			"/status Service status and active tasks\n" +
			"/users Allowed users (with one-tap removal)\n" +
			"/allow &lt;ID&gt; [note] Allow a user\n" +
			"/revoke &lt;ID&gt; Remove a user\n" +
			"/login Sign in to the Apple ID again"
	}
	return s
}

func renderPreview(info AppInfo, cached bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📱 <b>%s</b>\n", html.EscapeString(firstNonEmpty(info.Name, "Unknown app")))
	if info.Developer != "" {
		fmt.Fprintf(&sb, "👤 %s\n", html.EscapeString(info.Developer))
	}
	sb.WriteString("\n")
	version := firstNonEmpty(info.Version, "unknown")
	if !info.ReleaseDate.IsZero() {
		version += " (released " + info.ReleaseDate.Local().Format("Jan 2, 2006") + ")"
	}
	fmt.Fprintf(&sb, "Version: <b>%s</b>\n", html.EscapeString(version))
	fmt.Fprintf(&sb, "Bundle ID: <code>%s</code>\n", html.EscapeString(firstNonEmpty(info.BundleID, "unknown")))
	fmt.Fprintf(&sb, "App ID: <code>%d</code>\n", info.AppID)
	var extra []string
	if info.Size > 0 {
		extra = append(extra, "~"+formatBytes(info.Size))
	}
	switch {
	case info.PriceText != "":
		extra = append(extra, html.EscapeString(info.PriceText))
	case info.Price == 0:
		extra = append(extra, "Free")
	}
	if info.MinOS != "" {
		extra = append(extra, "iOS "+html.EscapeString(info.MinOS)+"+")
	}
	if len(extra) > 0 {
		sb.WriteString(strings.Join(extra, " · ") + "\n")
	}
	if cached {
		sb.WriteString("\n⚡ This version is cached. Confirm to get the link right away.")
	} else {
		sb.WriteString("\nIs this the right app? Confirm to download and decrypt the current version.")
	}
	return sb.String()
}

func appTitle(info AppInfo) string {
	t := "<b>" + html.EscapeString(firstNonEmpty(info.Name, info.BundleID, strconv.FormatInt(info.AppID, 10))) + "</b>"
	if info.Version != "" {
		t += " " + html.EscapeString(info.Version)
	}
	return t
}

func renderTask(info AppInfo, v TaskView, now time.Time) (string, Keyboard) {
	if v.Status == StageReady && v.Artifact != nil {
		return renderReady(info, *v.Artifact, false)
	}
	head := "📦 " + appTitle(info) + "\n\n"
	cancel := Keyboard{{{Text: "Cancel task", Data: callbackCancel + v.ID}}}
	elapsed := ""
	if v.StartedAt != nil {
		elapsed = " · " + formatDuration(now.Sub(*v.StartedAt)) + " elapsed"
	}
	switch v.Status {
	case StageQueued:
		return head + fmt.Sprintf("⏳ Queued · %d task(s) ahead", max(v.Position-1, 0)), cancel
	case StageDownloading:
		p := v.Progress
		if p == nil || p.Total == 0 {
			return head + "⬇️ Connecting to the App Store…" + elapsed, cancel
		}
		line := fmt.Sprintf("⬇️ Downloading <b>%.1f%%</b>\n%s\n%s / %s", p.Percent, progressBar(p.Percent), formatBytes(p.Bytes), formatBytes(p.Total))
		if p.SpeedBps > 0 {
			line += " · " + formatBytes(p.SpeedBps) + "/s"
		}
		if p.ETASeconds > 0 {
			line += " · " + formatDuration(time.Duration(p.ETASeconds)*time.Second) + " left"
		}
		return head + line, cancel
	case StagePatching:
		return head + "🧩 Downloaded. Adding license data…" + elapsed, cancel
	case StageWaitingDecrypt:
		return head + "⏳ Waiting to decrypt (one at a time)" + elapsed, cancel
	case StageDecrypting:
		return head + "🔓 Decrypting…" + elapsed, cancel
	case StageFinalizing:
		return head + "📦 Finishing up…" + elapsed, cancel
	case StageFailed:
		return head + "❌ Failed: " + friendlyError(v.Error), nil
	case StageCanceled:
		return head + "🚫 Task canceled", nil
	case StageReady:
		return head + "⌛ The file has expired. Send the app again.", nil
	}
	return head + string(v.Status), cancel
}

func renderReady(info AppInfo, a ArtifactView, cached bool) (string, Keyboard) {
	title := info
	if title.Version == "" {
		title.Version = a.Version
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ %s is ready", appTitle(title))
	if cached {
		sb.WriteString(" (cached)")
	}
	fmt.Fprintf(&sb, "\n\n📄 <code>%s</code>\n", html.EscapeString(a.FileName))
	fmt.Fprintf(&sb, "💾 %s\n", formatBytes(a.Size))
	fmt.Fprintf(&sb, "🔑 SHA-256: <code>%s</code>\n", a.SHA256)
	fmt.Fprintf(&sb, "⏰ Link valid until %s\n", a.ExpiresAt.Local().Format("Jan 2, 2006 15:04"))
	fmt.Fprintf(&sb, "\n<a href=\"%s\">Download</a>", html.EscapeString(a.DownloadURL))
	return sb.String(), Keyboard{{{Text: "⬇️ Download IPA", URL: a.DownloadURL}}}
}

// notice formats an admin notification. Notices share one header style so
// they stand apart from replies to the admin's own requests.
func notice(title string, lines ...string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s <b>Notice · %s</b>\n", noticeHeaderEmoji, html.EscapeString(title))
	var body []string
	for _, l := range lines {
		if l != "" {
			body = append(body, l)
		}
	}
	body = append(body, "🕒 "+time.Now().Format("2006-01-02 15:04:05"))
	sb.WriteString("<blockquote>" + strings.Join(body, "\n") + "</blockquote>")
	return sb.String()
}

func userLine(u tgUser) string {
	s := "👤 " + html.EscapeString(firstNonEmpty(u.DisplayName(), "No name"))
	if u.Username != "" {
		s += " (@" + html.EscapeString(u.Username) + ")"
	}
	return s + fmt.Sprintf(" · <code>%d</code>", u.ID)
}

func appLine(info AppInfo) string {
	s := "📱 " + appTitle(info)
	if info.BundleID != "" {
		s += " · <code>" + html.EscapeString(info.BundleID) + "</code>"
	}
	return s
}

func allowedLabel(u AllowedUser) string {
	s := html.EscapeString(firstNonEmpty(u.Name, "(hasn't messaged yet)"))
	if u.Username != "" {
		s += " @" + html.EscapeString(u.Username)
	}
	return s + fmt.Sprintf(" <code>%d</code>", u.ID)
}

var friendlyMessages = map[string]string{
	"not_found":             "The App Store has no such app. Check the ID, and whether the app is available in the account's region.",
	"license_required":      "The account doesn't own this app. Paid apps must be bought first.",
	"subscription_required": "This app requires a subscription to download.",
	"auth_required":         "The server's Apple ID session has expired. The admin has been notified.",
	"decrypt_failed":        "Decryption failed.",
	"version_mismatch":      "The downloaded version doesn't match the one you confirmed. Please send it again.",
	"timeout":               "The task timed out.",
	"canceled":              "The task was canceled.",
	"queue_full":            "The queue is full. Try again later.",
	"draining":              "The service is updating. Try again in a few minutes.",
	"unavailable":           "The App Store is temporarily unavailable. Try again later.",
	"bad_request":           "Invalid request.",
}

func friendlyError(e *TaskError) string {
	if e == nil {
		return "Unknown error."
	}
	msg, ok := friendlyMessages[e.Code]
	if !ok {
		msg = "Something went wrong."
	}
	detail := ""
	if e.Message != "" && e.Code != "canceled" && e.Code != "auth_required" {
		detail = "\n<code>" + html.EscapeString(truncate(e.Message, 300)) + "</code>"
	}
	return msg + detail
}

func progressBar(percent float64) string {
	filled := int(percent/10 + 0.5)
	filled = min(max(filled, 0), 10)
	return strings.Repeat("▰", filled) + strings.Repeat("▱", 10-filled)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusText renders /status for admins.
func statusText(m *Manager, c *Cache, h *Health, allow *Allowlist, versions map[string]string, account func() (string, error)) string {
	st := m.Status()
	cs := c.Stats()
	var sb strings.Builder
	sb.WriteString("📊 <b>Status</b>\n\n")
	if email, err := account(); err != nil {
		sb.WriteString("🍎 Apple ID: ⚠️ " + html.EscapeString(truncate(err.Error(), 120)) + "\n")
	} else {
		sb.WriteString("🍎 Apple ID: " + html.EscapeString(email) + "\n")
	}
	sb.WriteString("🩺 App Store: " + healthLine(h.State()) + "\n")
	fmt.Fprintf(&sb, "⚙️ %d running · %d queued", st.Running, st.Queued)
	if st.Draining {
		sb.WriteString(" · shutting down")
	}
	fmt.Fprintf(&sb, "\n💾 Cache: %d file(s) · %s", cs.Artifacts, formatBytes(cs.Bytes))
	if cs.MaxBytes > 0 {
		sb.WriteString(" / " + formatBytes(cs.MaxBytes))
	}
	fmt.Fprintf(&sb, "\n👥 %d allowed user(s)\n", len(allow.List()))
	keys := make([]string, 0, len(versions))
	for k := range versions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "🔧 %s <code>%s</code>\n", k, html.EscapeString(firstNonEmpty(versions[k], "unknown")))
	}
	var active []TaskView
	for _, v := range m.List() {
		if !v.Status.Terminal() {
			active = append(active, v)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].CreatedAt.Before(active[j].CreatedAt) })
	if len(active) > 0 {
		sb.WriteString("\n<b>Active tasks</b>\n")
		for _, v := range active {
			name := firstNonEmpty(v.App.Name, v.App.BundleID, strconv.FormatInt(v.App.AppID, 10))
			fmt.Fprintf(&sb, "• %s %s · %s", html.EscapeString(name), html.EscapeString(v.App.Version), stageLabel(v))
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func stageLabel(v TaskView) string {
	switch v.Status {
	case StageQueued:
		return fmt.Sprintf("queued #%d", v.Position)
	case StageDownloading:
		if v.Progress != nil {
			return fmt.Sprintf("downloading %.0f%%", v.Progress.Percent)
		}
		return "downloading"
	case StagePatching:
		return "adding license"
	case StageWaitingDecrypt:
		return "waiting to decrypt"
	case StageDecrypting:
		return "decrypting"
	case StageFinalizing:
		return "finishing"
	}
	return string(v.Status)
}
