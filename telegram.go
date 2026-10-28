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
	{Command: "start", Description: "使用说明"},
	{Command: "help", Description: "使用说明"},
}

var adminCommands = append(append([]botCommand{}, userCommands...),
	botCommand{Command: "status", Description: "服务状态与进行中的任务"},
	botCommand{Command: "users", Description: "白名单用户"},
	botCommand{Command: "allow", Description: "添加白名单：/allow <ID> [备注]"},
	botCommand{Command: "revoke", Description: "移除白名单：/revoke <ID>"},
	botCommand{Command: "login", Description: "重新登录 Apple 账号"},
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
	b.send(ctx, m.Chat.ID, fmt.Sprintf("🔒 你还没有使用权限。\n\n你的 Telegram ID：<code>%d</code>\n已通知管理员，获批后会收到消息。", user.ID), nil)
	b.mu.Lock()
	last, seen := b.requested[user.ID]
	if seen && b.now().Sub(last) < accessRequestGap {
		b.mu.Unlock()
		return
	}
	b.requested[user.ID] = b.now()
	b.mu.Unlock()
	id := strconv.FormatInt(user.ID, 10)
	b.notifyAdmins(ctx, 0, notice("访问申请", userLine(user), "💬 "+html.EscapeString(truncate(m.Text, 100))),
		Keyboard{{{Text: "✅ 允许", Data: callbackAllow + id}, {Text: "🚫 忽略", Data: callbackIgnore + id}}})
}

func (b *Bot) handleCommand(ctx context.Context, m *tgMessage, cmd, args string) {
	isAdmin := b.admins[m.From.ID]
	switch {
	case cmd == "start" || cmd == "help":
		b.send(ctx, m.Chat.ID, helpText(isAdmin), nil)
	case !isAdmin:
		b.send(ctx, m.Chat.ID, "这个命令只有管理员可以使用。", nil)
	case cmd == "status":
		b.send(ctx, m.Chat.ID, b.statusFn(), nil)
	case cmd == "users":
		b.sendUsers(ctx, m.Chat.ID)
	case cmd == "allow":
		fields := strings.Fields(args)
		id, err := strconv.ParseInt(firstOr(fields, ""), 10, 64)
		if err != nil || id <= 0 {
			b.send(ctx, m.Chat.ID, "用法：<code>/allow &lt;Telegram ID&gt; [备注]</code>", nil)
			return
		}
		note := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
		b.allowUser(ctx, m.Chat.ID, m.From.ID, AllowedUser{ID: id, Note: note})
	case cmd == "login":
		if code, ok := parseAuthCode(args); ok {
			if b.takeLoginWait(m.From.ID) {
				b.finishLogin(ctx, m.Chat.ID, code)
			} else {
				b.send(ctx, m.Chat.ID, "没有等待中的登录，请先发送 /login。", nil)
			}
			return
		}
		b.startLogin(ctx, m.Chat.ID, m.From.ID)
	case cmd == "revoke":
		id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
		if err != nil {
			b.send(ctx, m.Chat.ID, "用法：<code>/revoke &lt;Telegram ID&gt;</code>", nil)
			return
		}
		b.revokeUser(ctx, m.Chat.ID, id)
	default:
		b.send(ctx, m.Chat.ID, helpText(true), nil)
	}
}

func (b *Bot) allowUser(ctx context.Context, chatID, adminID int64, u AllowedUser) {
	if b.admins[u.ID] {
		b.send(ctx, chatID, "这是管理员，本来就有权限。", nil)
		return
	}
	u.AddedBy, u.AddedAt = adminID, b.now()
	added, err := b.allow.Allow(u)
	switch {
	case err != nil:
		b.send(ctx, chatID, "保存白名单失败："+html.EscapeString(err.Error()), nil)
	case !added:
		b.send(ctx, chatID, fmt.Sprintf("<code>%d</code> 已经在白名单里。", u.ID), nil)
	default:
		b.send(ctx, chatID, fmt.Sprintf("✅ 已添加 %s 到白名单。", allowedLabel(u)), nil)
		b.send(ctx, u.ID, "✅ 管理员已为你开通使用权限。\n\n"+helpText(false), nil)
	}
}

func (b *Bot) revokeUser(ctx context.Context, chatID, id int64) {
	u, removed, err := b.allow.Revoke(id)
	switch {
	case err != nil:
		b.send(ctx, chatID, "保存白名单失败："+html.EscapeString(err.Error()), nil)
	case !removed:
		b.send(ctx, chatID, fmt.Sprintf("<code>%d</code> 不在白名单里。", id), nil)
	default:
		b.send(ctx, chatID, fmt.Sprintf("已将 %s 移出白名单。", allowedLabel(u)), nil)
	}
}

func (b *Bot) sendUsers(ctx context.Context, chatID int64) {
	users := b.allow.List()
	if len(users) == 0 {
		b.send(ctx, chatID, "白名单为空。用 <code>/allow &lt;ID&gt;</code> 添加，或在访问申请通知上点「允许」。", nil)
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "👥 <b>白名单</b>（%d 人）\n", len(users))
	var kb Keyboard
	for _, u := range users {
		fmt.Fprintf(&sb, "\n• %s · 添加于 %s", allowedLabel(u), u.AddedAt.Local().Format("2006-01-02"))
		if u.Note != "" {
			fmt.Fprintf(&sb, "\n  备注：%s", html.EscapeString(u.Note))
		}
		kb = append(kb, []Button{{Text: "移除 " + truncate(firstNonEmpty(u.Name, u.Username, strconv.FormatInt(u.ID, 10)), 24), Data: callbackRevoke + strconv.FormatInt(u.ID, 10)}})
	}
	b.send(ctx, chatID, sb.String(), kb)
}

// preview resolves the app and asks the user to confirm the exact build.
func (b *Bot) preview(ctx context.Context, chatID int64, user tgUser, bundleID string, appID int64) {
	msgID := b.send(ctx, chatID, "🔍 正在查询 App Store…", nil)
	info, err := b.svc.Resolve(ctx, bundleID, appID)
	if err != nil {
		te := classify(err)
		b.edit(ctx, chatID, msgID, "❌ 查询失败："+friendlyError(te), nil)
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
	action := "✅ 下载并解密"
	if cached {
		action = "✅ 获取下载链接"
	}
	b.edit(ctx, chatID, msgID, renderPreview(info, cached),
		Keyboard{{{Text: action, Data: callbackOK + pid}, {Text: "❌ 取消", Data: callbackNo + pid}}})
}

func (b *Bot) handleCallback(ctx context.Context, cb *tgCallback) {
	answer := ""
	defer func() { _ = b.api.Answer(ctx, cb.ID, answer) }()
	if cb.Message == nil || !b.authorized(cb.From.ID) {
		answer = "没有权限"
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
			answer = "已过期，请重新发送"
			b.edit(ctx, chatID, msgID, "⌛ 确认已过期，请重新发送 Bundle ID 或 App ID。", nil)
			return
		}
		if kind == callbackNo {
			b.edit(ctx, chatID, msgID, renderPreview(p.info, false)+"\n\n🚫 已取消", nil)
			return
		}
		b.start(ctx, p, msgID)

	case callbackCancel:
		b.mu.Lock()
		following := b.watchers[arg][cb.From.ID]
		b.mu.Unlock()
		if !following && !isAdmin {
			answer = "只能取消自己的任务"
			return
		}
		if _, ok := b.svc.manager.Cancel(arg); !ok {
			answer = "任务不存在"
			return
		}
		answer = "正在取消…"

	case callbackAllow, callbackIgnore, callbackRevoke:
		if !isAdmin {
			answer = "只有管理员可以操作"
			return
		}
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return
		}
		switch kind {
		case callbackAllow:
			b.allowUser(ctx, chatID, cb.From.ID, AllowedUser{ID: id})
			b.edit(ctx, chatID, msgID, html.EscapeString(cb.Message.Text)+"\n\n✅ 已允许", nil)
		case callbackIgnore:
			b.edit(ctx, chatID, msgID, html.EscapeString(cb.Message.Text)+"\n\n🚫 已忽略", nil)
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
			b.notifyAdmins(ctx, user.ID, notice("新请求", userLine(user), appLine(info), "📌 命中缓存，已直接发送链接"), nil)
		}
		return
	}

	task := out.Task
	if !admin {
		how := "已创建任务"
		if !out.Created {
			how = "加入进行中的任务"
		}
		if task.Status == StageQueued {
			how += fmt.Sprintf("（排队第 %d 位）", task.Position)
		}
		b.notifyAdmins(ctx, user.ID, notice("新请求", userLine(user), appLine(info), "📌 "+how), nil)
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
		elapsed = "⏱ 用时 " + formatDuration(v.FinishedAt.Sub(*v.StartedAt))
	}
	switch v.Status {
	case StageReady:
		size := ""
		if v.Artifact != nil {
			size = " · " + formatBytes(v.Artifact.Size)
		}
		b.notifyAdmins(ctx, user.ID, notice("任务完成", userLine(user), appLine(info), elapsed+size), nil)
	case StageFailed:
		b.notifyAdmins(ctx, user.ID, notice("任务失败", userLine(user), appLine(info),
			fmt.Sprintf("❗ <code>%s</code> %s", v.Error.Code, html.EscapeString(truncate(v.Error.Message, 300)))), nil)
	case StageCanceled:
		b.notifyAdmins(ctx, user.ID, notice("任务取消", userLine(user), appLine(info)), nil)
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
	msgID := b.send(ctx, chatID, "🔐 正在重新登录 Apple 账号…", nil)
	email, needsCode, err := b.auth.StartLogin()
	switch {
	case err != nil:
		b.edit(ctx, chatID, msgID, "❌ 登录失败："+friendlyLoginError(err), nil)
	case needsCode:
		b.mu.Lock()
		b.loginWait[adminID] = b.now()
		b.mu.Unlock()
		b.edit(ctx, chatID, msgID, fmt.Sprintf("📲 Apple 已向 <b>%s</b> 的受信任设备发送验证码。\n\n请在 10 分钟内直接回复 6 位数字验证码。", html.EscapeString(email)), nil)
	default:
		b.edit(ctx, chatID, msgID, fmt.Sprintf("✅ 已重新登录 <b>%s</b>，正在复查下载链路…", html.EscapeString(email)), nil)
		b.recheck(ctx, chatID)
	}
}

func (b *Bot) finishLogin(ctx context.Context, chatID int64, code string) {
	msgID := b.send(ctx, chatID, "🔐 正在验证…", nil)
	email, err := b.auth.FinishLogin(code)
	if err != nil {
		b.edit(ctx, chatID, msgID, "❌ 登录失败："+friendlyLoginError(err)+"\n\n可以重新发送 /login。", nil)
		return
	}
	b.edit(ctx, chatID, msgID, fmt.Sprintf("✅ 已重新登录 <b>%s</b>，正在复查下载链路…", html.EscapeString(email)), nil)
	b.recheck(ctx, chatID)
}

// recheck runs the health check now and reports the result to chatID; other
// admins hear about a recovery through the usual change notice.
func (b *Bot) recheck(ctx context.Context, chatID int64) {
	if h := b.svc.health; h != nil && h.check != nil {
		b.send(ctx, chatID, "🩺 下载链路："+healthLine(h.CheckNow(ctx)), nil)
	}
}

func friendlyLoginError(err error) string {
	if errors.Is(err, errNoCredentials) {
		return "没有可用的账号密码：设置 UNFAIRPLAY_BOT_APPLE_ID_EMAIL / UNFAIRPLAY_BOT_APPLE_ID_PASSWORD，或在服务器上执行 <code>ipatool auth login</code>。"
	}
	return "<code>" + html.EscapeString(truncate(err.Error(), 300)) + "</code>"
}

// HealthChanged notifies admins when the App Store side breaks or recovers.
func (b *Bot) HealthChanged(_, cur HealthState) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cur.OK {
		b.notifyAdmins(ctx, 0, notice("下载链路已恢复", "✅ App Store 登录与下载接口正常"), nil)
		return
	}
	hint := "可能是 Apple 改了接口。ipatool 每 6 小时自动跟进上游，修复发布后服务器会自动更新。"
	if cur.Error.Code == "auth_required" {
		hint = "Apple 账号登录失效且自动重登未成功，检查环境变量中的密码，或发送 /login 重试。"
	}
	b.notifyAdmins(ctx, 0, notice("下载链路异常",
		fmt.Sprintf("❗ <code>%s</code> %s", cur.Error.Code, html.EscapeString(truncate(cur.Error.Message, 300))),
		"👉 "+hint), nil)
}

func healthLine(st HealthState) string {
	switch {
	case !st.Enabled:
		return "未启用（设置 UNFAIRPLAY_BOT_HEALTH_APP_ID）"
	case st.CheckedAt.IsZero():
		return "尚未检查"
	case st.OK:
		return "✅ 正常（" + st.CheckedAt.Local().Format("01-02 15:04") + " 检查）"
	}
	return fmt.Sprintf("❌ 异常 <code>%s</code>（自 %s）", st.Error.Code, st.Since.Local().Format("01-02 15:04"))
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
		"发送以下任意一种，我会先查出 App 信息让你确认：\n" +
		"• App ID：<code>414478124</code>\n" +
		"• Bundle ID：<code>com.tencent.xin</code>\n" +
		"• App Store 链接：<code>https://apps.apple.com/app/id414478124</code>\n\n" +
		"确认后会下载当前最新版本并解密，完成后发送下载链接。\n" +
		"同一版本已有缓存时会直接给出链接。"
	if admin {
		s += "\n\n<b>管理员命令</b>\n" +
			"/status 服务状态与进行中的任务\n" +
			"/users 白名单（可一键移除）\n" +
			"/allow &lt;ID&gt; [备注] 添加白名单\n" +
			"/revoke &lt;ID&gt; 移除白名单\n" +
			"/login 重新登录 Apple 账号"
	}
	return s
}

func renderPreview(info AppInfo, cached bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📱 <b>%s</b>\n", html.EscapeString(firstNonEmpty(info.Name, "未知名称")))
	if info.Developer != "" {
		fmt.Fprintf(&sb, "👤 %s\n", html.EscapeString(info.Developer))
	}
	sb.WriteString("\n")
	version := firstNonEmpty(info.Version, "未知")
	if !info.ReleaseDate.IsZero() {
		version += "（" + info.ReleaseDate.Local().Format("2006-01-02") + " 发布）"
	}
	fmt.Fprintf(&sb, "版本：<b>%s</b>\n", html.EscapeString(version))
	fmt.Fprintf(&sb, "Bundle ID：<code>%s</code>\n", html.EscapeString(firstNonEmpty(info.BundleID, "未知")))
	fmt.Fprintf(&sb, "App ID：<code>%d</code>\n", info.AppID)
	var extra []string
	if info.Size > 0 {
		extra = append(extra, "约 "+formatBytes(info.Size))
	}
	switch {
	case info.PriceText != "":
		extra = append(extra, html.EscapeString(info.PriceText))
	case info.Price == 0:
		extra = append(extra, "免费")
	}
	if info.MinOS != "" {
		extra = append(extra, "iOS "+html.EscapeString(info.MinOS)+"+")
	}
	if len(extra) > 0 {
		sb.WriteString(strings.Join(extra, " · ") + "\n")
	}
	if cached {
		sb.WriteString("\n⚡ 这个版本已有缓存，确认后立即给出链接。")
	} else {
		sb.WriteString("\n是这个 App 吗？确认后开始下载并解密最新版本。")
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
	cancel := Keyboard{{{Text: "取消任务", Data: callbackCancel + v.ID}}}
	elapsed := ""
	if v.StartedAt != nil {
		elapsed = " · 已用 " + formatDuration(now.Sub(*v.StartedAt))
	}
	switch v.Status {
	case StageQueued:
		return head + fmt.Sprintf("⏳ 排队中 · 前面还有 %d 个任务", max(v.Position-1, 0)), cancel
	case StageDownloading:
		p := v.Progress
		if p == nil || p.Total == 0 {
			return head + "⬇️ 正在连接 App Store…" + elapsed, cancel
		}
		line := fmt.Sprintf("⬇️ 下载中 <b>%.1f%%</b>\n%s\n%s / %s", p.Percent, progressBar(p.Percent), formatBytes(p.Bytes), formatBytes(p.Total))
		if p.SpeedBps > 0 {
			line += " · " + formatBytes(p.SpeedBps) + "/s"
		}
		if p.ETASeconds > 0 {
			line += " · 剩余 " + formatDuration(time.Duration(p.ETASeconds)*time.Second)
		}
		return head + line, cancel
	case StagePatching:
		return head + "🧩 下载完成，正在写入授权信息…" + elapsed, cancel
	case StageWaitingDecrypt:
		return head + "⏳ 等待解密（同一时间只解密一个）" + elapsed, cancel
	case StageDecrypting:
		return head + "🔓 解密中…" + elapsed, cancel
	case StageFinalizing:
		return head + "📦 正在整理文件…" + elapsed, cancel
	case StageFailed:
		return head + "❌ 失败：" + friendlyError(v.Error), nil
	case StageCanceled:
		return head + "🚫 任务已取消", nil
	case StageReady:
		return head + "⌛ 文件已过期，请重新发送。", nil
	}
	return head + string(v.Status), cancel
}

func renderReady(info AppInfo, a ArtifactView, cached bool) (string, Keyboard) {
	title := info
	if title.Version == "" {
		title.Version = a.Version
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ %s 已就绪", appTitle(title))
	if cached {
		sb.WriteString("（缓存）")
	}
	fmt.Fprintf(&sb, "\n\n📄 <code>%s</code>\n", html.EscapeString(a.FileName))
	fmt.Fprintf(&sb, "💾 %s\n", formatBytes(a.Size))
	fmt.Fprintf(&sb, "🔑 SHA-256：<code>%s</code>\n", a.SHA256)
	fmt.Fprintf(&sb, "⏰ 链接有效期至 %s\n", a.ExpiresAt.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(&sb, "\n<a href=\"%s\">点此下载</a>", html.EscapeString(a.DownloadURL))
	return sb.String(), Keyboard{{{Text: "⬇️ 下载 IPA", URL: a.DownloadURL}}}
}

// notice formats an admin notification. Notices share one header style so
// they stand apart from replies to the admin's own requests.
func notice(title string, lines ...string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s <b>通知 · %s</b>\n", noticeHeaderEmoji, html.EscapeString(title))
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
	s := "👤 " + html.EscapeString(firstNonEmpty(u.DisplayName(), "无名"))
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
	s := html.EscapeString(firstNonEmpty(u.Name, "（尚未互动）"))
	if u.Username != "" {
		s += " @" + html.EscapeString(u.Username)
	}
	return s + fmt.Sprintf(" <code>%d</code>", u.ID)
}

var friendlyMessages = map[string]string{
	"not_found":             "App Store 找不到这个 App（ID 是否正确？是否在账号所在地区上架？）",
	"license_required":      "账号没有这个 App 的授权（付费 App 需要先购买）",
	"subscription_required": "这个 App 需要订阅才能下载",
	"auth_required":         "服务器上的 Apple 账号登录已失效，已通知管理员",
	"decrypt_failed":        "解密失败",
	"version_mismatch":      "下载到的版本和确认的不一致，请重新发送",
	"timeout":               "任务超时",
	"canceled":              "任务已取消",
	"queue_full":            "队列已满，请稍后再试",
	"draining":              "服务正在更新，请稍后再试",
	"unavailable":           "App Store 暂时不可用，请稍后再试",
	"bad_request":           "请求有误",
}

func friendlyError(e *TaskError) string {
	if e == nil {
		return "未知错误"
	}
	msg, ok := friendlyMessages[e.Code]
	if !ok {
		msg = "出错了"
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
		return fmt.Sprintf("%d小时%d分", h, m)
	case m > 0:
		return fmt.Sprintf("%d分%d秒", m, s)
	}
	return fmt.Sprintf("%d秒", s)
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
	sb.WriteString("📊 <b>服务状态</b>\n\n")
	if email, err := account(); err != nil {
		sb.WriteString("🍎 Apple 账号：⚠️ " + html.EscapeString(truncate(err.Error(), 120)) + "\n")
	} else {
		sb.WriteString("🍎 Apple 账号：" + html.EscapeString(email) + "\n")
	}
	sb.WriteString("🩺 下载链路：" + healthLine(h.State()) + "\n")
	fmt.Fprintf(&sb, "⚙️ 运行中 %d · 排队 %d", st.Running, st.Queued)
	if st.Draining {
		sb.WriteString(" · 正在停机")
	}
	fmt.Fprintf(&sb, "\n💾 缓存 %d 个文件 · %s", cs.Artifacts, formatBytes(cs.Bytes))
	if cs.MaxBytes > 0 {
		sb.WriteString(" / " + formatBytes(cs.MaxBytes))
	}
	fmt.Fprintf(&sb, "\n👥 白名单 %d 人\n", len(allow.List()))
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
		sb.WriteString("\n<b>进行中的任务</b>\n")
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
		return fmt.Sprintf("排队第 %d 位", v.Position)
	case StageDownloading:
		if v.Progress != nil {
			return fmt.Sprintf("下载 %.0f%%", v.Progress.Percent)
		}
		return "下载中"
	case StagePatching:
		return "写入授权"
	case StageWaitingDecrypt:
		return "等待解密"
	case StageDecrypting:
		return "解密中"
	case StageFinalizing:
		return "整理文件"
	}
	return string(v.Status)
}
