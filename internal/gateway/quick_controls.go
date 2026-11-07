package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

type quickControl struct {
	Room          string `json:"room"`
	Session       string `json:"session"`
	Revision      string `json:"revision"`
	Turn          string `json:"turn,omitempty"`
	Output        string `json:"output,omitempty"`
	TargetSession string `json:"targetSession,omitempty"`
	Query         string `json:"query,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
	Action        string `json:"action"`
}

func messageReply(m core.Message) string {
	if m.InteractionMessageID != "" {
		return m.InteractionMessageID
	}
	return m.ID
}

func (g *Gateway) signQuick(p quickControl) string {
	data, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, g.historyKey[:])
	_, _ = mac.Write([]byte("quick/v1\x00"))
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (g *Gateway) readQuick(token string) (quickControl, bool) {
	var p quickControl
	if len(token) > 4096 {
		return p, false
	}
	payload, signature, ok := strings.Cut(token, ".")
	if !ok {
		return p, false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return p, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return p, false
	}
	mac := hmac.New(sha256.New, g.historyKey[:])
	_, _ = mac.Write([]byte("quick/v1\x00"))
	_, _ = mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(data, &p) != nil || p.Room == "" || p.Session == "" || p.Revision == "" {
		return quickControl{}, false
	}
	return p, true
}

// Identical render context produces identical JSON. In particular, wall time
// must not invalidate streaming snapshot deduplication or a frozen create body.
func (g *Gateway) quickControls(r core.Room, session, turn, output string) []any {
	return g.quickControlsRevision(r, session, turn, output, "")
}

// A later failed delivery attempt gets new controls, while duplicate clicks on
// the same visible attempt remain one-shot. The outbox ID itself stays stable.
func (g *Gateway) quickOutputControls(r core.Room, session, output string, attempt int) []any {
	return g.quickControlsRevision(r, session, "", output, "retry:"+strconv.Itoa(attempt))
}

func (g *Gateway) quickControlsRevision(r core.Room, session, turn, output, salt string) []any {
	if session == "" {
		return nil
	}
	p := quickControl{Room: r.Key, Session: session, Turn: turn, Output: output}
	p.Revision = core.Hash(jsonText([]string{r.Key, session, turn, output, salt}))[:32]
	makeButton := func(action, label, style string) core.Args {
		q := p
		q.Action = action
		b := button(label, nil, style)
		delete(b, "value")
		element := action
		if element == "supplement" {
			element = "extra"
		}
		b["element_id"] = "gateway_quick_" + element
		b["behaviors"] = []any{core.Args{"type": "callback", "value": core.Args{"quick_control": g.signQuick(q)}}}
		return b
	}
	buttons := []any{makeButton("status", "查看状态", "default"), makeButton("answer", "待回答", "default")}
	if turn != "" {
		buttons = append(buttons, makeButton("stop", "停止本轮", "danger"))
	}
	if output != "" {
		b := makeButton("retry", "重试交付", "default")
		b["confirm"] = core.Args{"title": core.Args{"tag": "plain_text", "content": "重试已保存输出"}, "text": core.Args{"tag": "plain_text", "content": "仅补发保存的输出；此前结果未知时可能重复。"}}
		buttons = append(buttons, b)
	}
	columns := []any{}
	for _, b := range buttons {
		columns = append(columns, core.Args{"tag": "column", "width": "auto", "elements": []any{b}})
	}
	result := []any{
		core.Args{"tag": "hr", "element_id": "gateway_quick_div"},
		core.Args{"tag": "column_set", "element_id": "gateway_quick_row", "flex_mode": "none", "horizontal_spacing": "medium", "columns": columns},
	}
	if turn != "" {
		submit := makeButton("supplement", "发送", "primary_filled")
		submit["name"] = "quick_supplement_submit"
		submit["form_action_type"] = "submit"
		result = append(result, core.Args{"tag": "form", "element_id": "gateway_quick_form", "name": "quick_supplement_form", "elements": []any{
			core.Args{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "medium", "columns": []any{
				core.Args{"tag": "column", "width": "weighted", "weight": 1, "vertical_align": "bottom", "elements": []any{
					core.Args{"tag": "input", "name": "quick_supplement", "input_type": "multiline_text", "rows": 2, "width": "fill", "max_length": min(1000, g.config.Limits.TextChars), "required": true, "placeholder": core.Args{"tag": "plain_text", "content": "仅补充这次正在执行的前台任务"}},
				}},
				core.Args{"tag": "column", "width": "auto", "vertical_align": "bottom", "elements": []any{submit}},
			}},
		}})
	}
	return result
}

func replaceQuickControls(card core.Args, controls []any) core.Args {
	data, err := json.Marshal(card)
	if err != nil {
		return card
	}
	copy := core.Args{}
	if json.Unmarshal(data, &copy) != nil {
		return card
	}
	body := argsOf(copy["body"])
	if body == nil {
		return copy
	}
	old, _ := body["elements"].([]any)
	elements := make([]any, 0, len(old)+len(controls))
	for _, element := range old {
		if !strings.HasPrefix(stringArg(argsOf(element), "element_id"), "gateway_quick_") {
			elements = append(elements, element)
		}
	}
	body["elements"] = append(elements, controls...)
	copy["body"] = body
	return copy
}

// Bind only after successful platform delivery and outside an existing store
// transaction (the store deliberately owns one SQLite connection).
func (g *Gateway) bindQuickControls(message string, card core.Args) error {
	if message == "" {
		return nil
	}
	var found *quickControl
	mismatch := false
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if token, ok := v["quick_control"].(string); ok {
				if p, valid := g.readQuick(token); valid {
					if found != nil && (found.Room != p.Room || found.Session != p.Session || found.Revision != p.Revision) {
						mismatch = true
					}
					q := p
					found = &q
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(card)
	if mismatch {
		return errors.New("card controls refer to multiple revisions")
	}
	if found == nil {
		_, err := g.store.DB.Exec("DELETE FROM card_controls WHERE message=?", message)
		return err
	}
	now := time.Now().UnixMilli()
	return g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM card_controls WHERE expires<=?", now); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM card_control_uses WHERE message=? AND revision!=?", message, found.Revision); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO card_controls(message,room,session,revision,expires) VALUES(?,?,?,?,?) ON CONFLICT(message) DO UPDATE SET room=excluded.room,session=excluded.session,revision=excluded.revision,expires=excluded.expires`, message, found.Room, found.Session, found.Revision, now+int64(24*time.Hour/time.Millisecond))
		return err
	})
}

func (g *Gateway) quickBound(a core.CardAction, p quickControl) bool {
	var n int
	err := g.store.DB.QueryRow(`SELECT count(*) FROM card_controls c JOIN sessions s ON s.id=c.session AND s.room=c.room WHERE c.message=? AND c.room=? AND c.session=? AND c.revision=? AND c.expires>? AND s.state='open'`, a.MessageID, p.Room, p.Session, p.Revision, time.Now().UnixMilli()).Scan(&n)
	return err == nil && n == 1
}

func (g *Gateway) claimQuick(a core.CardAction, p quickControl) (bool, error) {
	result, err := g.store.DB.Exec(`INSERT OR IGNORE INTO card_control_uses(message,revision,action) SELECT message,revision,? FROM card_controls WHERE message=? AND room=? AND session=? AND revision=? AND expires>?`, p.Action, a.MessageID, p.Room, p.Session, p.Revision, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (g *Gateway) quickRate(r core.Room, actor string) bool {
	now := time.Now().UnixMilli()
	g.mu.Lock()
	defer g.mu.Unlock()
	// Same budget as messages: per chat and author across all topics.
	key := r.WorkspaceKey() + ":" + actor
	recent := g.rates[key][:0]
	for _, at := range g.rates[key] {
		if at > now-60000 {
			recent = append(recent, at)
		}
	}
	allowed := len(recent) < g.config.Limits.MessagesPerMinute
	if allowed {
		recent = append(recent, now)
	}
	g.rates[key] = recent
	return allowed
}

func (g *Gateway) quickAction(ctx context.Context, a core.CardAction) (string, bool, error) {
	token := stringArg(a.Value, "quick_control")
	if token == "" {
		return "", false, nil
	}
	p, valid := g.readQuick(token)
	if !valid {
		return "此快捷操作已失效，请使用最新卡片或命令。", true, nil
	}
	r, allowed := RoomFromKey(g.config, p.Room)
	if !allowed || a.ChatID != r.ChatID || a.MessageID == "" || !AuthorizedAuthor(r, a.UserID) {
		return "", true, nil
	}
	if !g.quickBound(a, p) {
		return "卡片已刷新、过期或会话已切换，请使用最新卡片。", true, nil
	}
	if (p.Action == "stop" || p.Action == "retry" || p.Action == "resume") && !canManage(r, a.UserID) {
		return "此操作需要本聊天维护者。", true, nil
	}
	if p.Action != "stop" && !g.quickRate(r, a.UserID) {
		return "操作过于频繁，请稍后再试。", true, nil
	}
	m := core.Message{ID: a.MessageID, ChatID: r.ChatID, SenderID: a.UserID}
	switch p.Action {
	case "status":
		g.task(func() { g.refreshStatusCard(r, a.UserID) })
		return "正在刷新状态卡片。", true, nil
	case "answer":
		g.approvalMu.Lock()
		ids := []string{}
		for id, request := range g.approvals {
			if request.room.Key == r.Key && request.session == p.Session && request.kind == "question" && request.expires > time.Now().UnixMilli() {
				ids = append(ids, id)
			}
		}
		g.approvalMu.Unlock()
		if len(ids) == 0 {
			return "当前没有待回答问题；审批仍需使用相应审批卡。", true, nil
		}
		sort.Strings(ids)
		lines := []string{"请在最新追问卡选择并提交；也可回复："}
		for _, id := range ids {
			lines = append(lines, "`/answer "+id+" 回答正文`")
		}
		g.notify(r, strings.Join(lines, "\n"), &m)
		return "已发送当前问题的回答入口。", true, nil
	case "supplement":
		text, err := g.quickSupplement(ctx, r, a, p)
		return text, true, err
	case "sessions":
		g.task(func() {
			if !g.quickBound(a, p) {
				return
			}
			m.Text = "/sessions " + p.Query
			if p.Cursor != "" {
				m.Text = "/sessions --cursor " + p.Cursor
			}
			if err := g.sessionsReply(g.ctx, r, m); err != nil {
				g.audit(r.Key, "session_page_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
			}
		})
		return "已请求会话列表。", true, nil
	case "resume":
		id, err := g.store.ResolveSession(r.Key, p.TargetSession)
		if err != nil || p.TargetSession == "" || id != p.TargetSession || id == p.Session {
			return "所选历史会话不存在或不属于当前聊天。", true, nil
		}
		claimed, err := g.claimQuick(a, p)
		if err != nil || !claimed {
			return "会话恢复已提交或卡片已失效。", true, err
		}
		g.task(func() {
			unlock := g.inputLocks.lock(r.Key)
			defer unlock()
			if !g.quickBound(a, p) {
				return
			}
			ctx, cancel := context.WithTimeout(g.ctx, 15*time.Second)
			defer cancel()
			m.Text = "/resume " + p.TargetSession
			if err := g.command(ctx, r, m); err != nil {
				g.audit(r.Key, "quick_resume_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
			}
		})
		return "已请求停止当前运行并选择历史会话；下一条消息才恢复模型。", true, nil
	case "stop":
		target := g.foregroundTarget(r)
		if p.Turn == "" || target == nil || target.UUID != p.Turn || target.SessionID != p.Session {
			return "本轮已经结束；旧按钮不会停止后续任务。", true, nil
		}
		claimed, err := g.claimQuick(a, p)
		if err != nil || !claimed {
			return "停止请求已提交或卡片已失效。", true, err
		}
		g.task(func() { g.quickStop(r, a, p) })
		return "已请求停止这次前台运行。", true, nil
	case "retry":
		o, err := g.output(p.Output)
		if err != nil || p.Output == "" || o.Room != r.Key || o.Session != p.Session || !g.outputAllowed(o, r) {
			return "交付记录不存在或已失效。", true, nil
		}
		if !contains([]string{"pending", "uncertain", "blocked"}, o.State) {
			return "输出已送达或正在发送，无需重复提交。", true, nil
		}
		claimed, err := g.claimQuick(a, p)
		if err != nil || !claimed {
			return "补发已提交；后续操作请查看 /outputs。", true, err
		}
		g.task(func() {
			if g.quickBound(a, p) {
				_, _ = g.deliverOutput(g.ctx, p.Output, r, true)
			}
		})
		return "已请求补发保存的输出；结果未知时可能重复。", true, nil
	}
	return "无效快捷操作。", true, nil
}

func (g *Gateway) quickStop(r core.Room, a core.CardAction, p quickControl) {
	unlock := g.inputLocks.lock(r.Key)
	defer unlock()
	if !g.quickBound(a, p) {
		return
	}
	s := g.getSlot(r.Key)
	if s == nil {
		return
	}
	s.events.Lock()
	s.mu.Lock()
	eligible := s.live && s.busy && s.session.ID == p.Session && s.anchor == p.Turn && s.compacting == ""
	if eligible {
		// Invalidate while holding the native event lock: a result cannot advance
		// to a later turn between the stale check and stopRoom's cancellation.
		s.live = false
		s.cancel()
	}
	s.mu.Unlock()
	s.events.Unlock()
	if !eligible {
		return
	}
	ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
	err := g.stopRoom(ctx, r)
	cancel()
	if err != nil {
		text := "停止尚未确认，请检查 /status 和服务诊断。"
		if errors.Is(err, errStopState) {
			text = stopStateNotice
		}
		g.notify(r, text, nil)
		return
	}
	g.notify(r, "已停止本聊天运行，保留原生上下文和会话 ID。", nil)
}

func (g *Gateway) quickSupplement(ctx context.Context, r core.Room, a core.CardAction, p quickControl) (string, error) {
	text := strings.TrimSpace(stringArg(a.FormValue, "quick_supplement"))
	if text == "" || utf8.RuneCountInString(text) > min(1000, g.config.Limits.TextChars) {
		return "请填写补充正文（最多 1000 字）；更长内容请使用 /btw。", nil
	}
	unlock, err := g.inputLocks.lockContext(ctx, r.Key)
	if err != nil {
		return "请稍后重新提交。", err
	}
	defer unlock()
	if !g.quickBound(a, p) {
		return "卡片已更新，请重新填写最新卡片。", nil
	}
	s := g.getSlot(r.Key)
	if s == nil {
		return "本轮已经结束，补充未提交。", nil
	}
	// The event lock is held while a result is persisted; wait for it only
	// within the callback deadline.
	if !lockWithin(ctx, &s.events) {
		return "请稍后重新提交。", ctx.Err()
	}
	defer s.events.Unlock()
	target := g.foregroundTarget(r)
	if p.Turn == "" || target == nil || target.UUID != p.Turn || target.SessionID != p.Session {
		return "本轮已经结束，补充未提交；如需新任务请发送普通消息。", nil
	}
	id := "card:" + core.Hash(jsonText([]string{a.MessageID, p.Revision, a.UserID, text}))
	seen, err := g.store.Seen(r.Key, id)
	if err != nil {
		return "", err
	}
	if seen {
		return "这段补充已接收，无需重复提交。", nil
	}
	n, err := g.pendingInChat(r)
	if err != nil {
		return "", err
	}
	if n >= g.config.Limits.InboxPerRoom {
		return "本聊天待交付输入已达上限，请查看 /status。", nil
	}
	now := time.Now().UnixMilli()
	m := core.Message{ID: id, InteractionMessageID: a.MessageID, ChatID: r.ChatID, ChatType: "group", SenderID: a.UserID, Text: text, Time: now, Mentioned: true, Resources: []core.Resource{}}
	if r.DMUser != "" {
		m.ChatType = "p2p"
	}
	d := core.Delivery{UUID: core.NewID(), RoomKey: r.Key, SessionID: p.Session, Message: m, SupplementTo: p.Turn}
	if err := ctx.Err(); err != nil {
		// Feishu has already reported a timeout to the user; do not commit.
		return "请稍后重新提交。", err
	}
	if err := g.queueInput(d, now); err != nil {
		return "", err
	}
	g.kick(r)
	return "补充已保存，等待交给本轮原生输入流。", nil
}
