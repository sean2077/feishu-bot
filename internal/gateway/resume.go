package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const resumeInterruptedPrompt = "上次回答被中断，请基于已有上下文继续完成刚才的问题。中断前的写操作未重放，请先核实。"

const resumeRequestLimit = 300

func resumePrompt(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return resumeInterruptedPrompt
	}
	return "上次回答被中断。请只继续完成这条原始请求，不要切换到历史中的其它中断点：\n\n" + cut(text, resumeRequestLimit) + "\n\n中断前的写操作未重放，请先核实。"
}

func (g *Gateway) resumeHint(show bool, r core.Room) string {
	if !show {
		return ""
	}
	if r.DMUser != "" || r.MentionOptional {
		return "点击卡片上的「继续」，或直接发送任意内容即可继续。"
	}
	return "点击卡片上的「继续」，或在**本话题 @" + escapeCardTitle(g.cardTitle()) + "**发送任意内容；直接回复不带 @ 不会触发。"
}

func (g *Gateway) resumeBlocked(r core.Room, s *slot) bool {
	g.mu.Lock()
	blocked := g.stopBlocked[r.Key]
	g.mu.Unlock()
	if blocked {
		return true
	}
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopUnconfirmed
	}
	return false
}

func (g *Gateway) resumeAvailable(r core.Room, session string) bool {
	return g.resumeAvailableFor(r, session, "")
}

func (g *Gateway) resumeAvailableFor(r core.Room, session, source string) bool {
	if session == "" {
		return false
	}
	_, ok := g.resumeRequest(r.Key, session, source)
	return ok
}

// resumeRequest resolves the request a card was rendered for. A closing status
// card points at its interrupted output; recovery cards fall back to the
// newest uncertain input in the session.
func (g *Gateway) resumeRequest(room, session, source string) (core.Message, bool) {
	if source != "" {
		var reply string
		err := g.store.DB.QueryRow("SELECT reply_to FROM outbox WHERE id=? AND room=? AND session=? AND kind='status' AND reply_to IS NOT NULL", source, room, session).Scan(&reply)
		if err == nil && reply != "" {
			if m, ok := g.inboxMessage(room, session, reply); ok {
				return m, true
			}
			if m, ok := g.inboxInteractionMessage(room, session, reply); ok {
				return m, true
			}
		}
		return core.Message{}, false
	}
	return g.uncertainInput(room, session)
}

func (g *Gateway) inboxMessage(room, session, id string) (core.Message, bool) {
	var raw string
	err := g.store.DB.QueryRow("SELECT message FROM inbox WHERE room=? AND session=? AND id=?", room, session, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return core.Message{}, false
	}
	var m core.Message
	if json.Unmarshal([]byte(raw), &m) != nil {
		return core.Message{}, false
	}
	return m, true
}

// A resumed turn replies to the card that was clicked. That card is not an
// inbox row, but the synthetic resume input carries it as its interaction
// message ID; this keeps a second interruption anchorable.
func (g *Gateway) inboxInteractionMessage(room, session, interaction string) (core.Message, bool) {
	var raw string
	err := g.store.DB.QueryRow("SELECT message FROM inbox WHERE room=? AND session=? AND json_valid(message) AND json_extract(message,'$.interactionMessageId')=? ORDER BY at DESC,id DESC LIMIT 1", room, session, interaction).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return core.Message{}, false
	}
	var m core.Message
	if json.Unmarshal([]byte(raw), &m) != nil {
		return core.Message{}, false
	}
	return m, true
}

func (g *Gateway) uncertainInput(room, session string) (core.Message, bool) {
	var raw string
	err := g.store.DB.QueryRow("SELECT message FROM inbox WHERE room=? AND session=? AND status='uncertain' ORDER BY at DESC,id DESC LIMIT 1", room, session).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return core.Message{}, false
	}
	var m core.Message
	if json.Unmarshal([]byte(raw), &m) != nil {
		return core.Message{}, false
	}
	return m, true
}

func (g *Gateway) resumeControls(r core.Room, session, source string) []any {
	p := quickControl{Room: r.Key, Session: session, Output: source, Revision: g.quickRevision(r, session, "", source, ""), Action: "resume_interrupted"}
	b := button("继续", nil, "primary_filled")
	delete(b, "value")
	b["element_id"] = "gateway_quick_resume"
	b["behaviors"] = []any{core.Args{"type": "callback", "value": core.Args{"quick_control": g.signQuick(p)}}}
	return []any{
		core.Args{"tag": "hr", "element_id": "gateway_quick_rhr"},
		core.Args{"tag": "column_set", "element_id": "gateway_quick_rrow", "flex_mode": "none", "horizontal_spacing": "medium", "columns": []any{
			core.Args{"tag": "column", "width": "auto", "elements": []any{b}},
		}},
	}
}

func (g *Gateway) resumeInterrupted(ctx context.Context, r core.Room, a core.CardAction, p quickControl) (string, bool, error) {
	unlock := g.inputLocks.lock(r.Key)
	defer unlock()
	original, ok := g.resumeRequest(r.Key, p.Session, p.Output)
	if !ok {
		return "当前没有可继续的中断输入。", true, nil
	}
	if a.UserID != original.SenderID && !contains(r.Maintainers, a.UserID) {
		return "只有原提问人或维护者可以继续这次回答。", true, nil
	}
	if g.resumeBlocked(r, g.getSlot(r.Key)) {
		return "当前聊天处于暂停或停止未确认状态，暂时不能继续。", true, nil
	}
	claimed, err := g.claimQuick(ctx, a, p)
	if err != nil {
		return "", true, err
	}
	if !claimed {
		return "已继续或卡片已失效。", true, nil
	}
	now := time.Now().UnixMilli()
	chatType := "group"
	if r.DMUser != "" {
		chatType = "p2p"
	}
	// Keep the original requester's identity on the synthetic turn so a later
	// interruption remains resumable by that user, even if a maintainer clicked.
	sender := original.SenderID
	if sender == "" {
		sender = a.UserID
	}
	m := core.Message{ID: core.NewID(), ChatID: r.ChatID, ChatType: chatType, SenderID: sender, Text: resumePrompt(original.Text), Time: now, Mentioned: true, ThreadID: original.ThreadID, RootID: original.RootID, InteractionMessageID: a.MessageID, Resources: []core.Resource{}}
	d := core.Delivery{UUID: core.NewID(), RoomKey: r.Key, SessionID: p.Session, Message: m}
	g.mu.Lock()
	err = g.queueInput(ctx, d, now)
	g.mu.Unlock()
	if err != nil {
		g.releaseQuick(a, p)
		if errors.Is(err, errInboxFull) {
			return "待交付输入已满，未能继续。", true, nil
		}
		return "", true, err
	}
	g.acceptReceipt(r, p.Session, m)
	g.kick(r)
	g.markResumeUsed(ctx, a.MessageID)
	return "已继续。中断前的写操作未重放，请先核实。", true, nil
}

func (g *Gateway) releaseQuick(a core.CardAction, p quickControl) {
	_, _ = g.store.DB.Exec("DELETE FROM card_control_uses WHERE message=? AND revision=? AND action=?", a.MessageID, p.Revision, p.Action)
}

// markResumeUsed is cosmetic and best-effort. claimQuick already made the
// callback one-shot even when the card is not a CardKit entity.
func (g *Gateway) markResumeUsed(ctx context.Context, message string) {
	updater, ok := g.transport.(core.CardElementUpdater)
	if !ok {
		return
	}
	element := core.Args{"tag": "button", "element_id": "gateway_quick_resume", "type": "default", "text": core.Args{"tag": "plain_text", "content": "已继续"}}
	unlock := g.feedbackLocks.lock(message)
	defer unlock()
	_ = updater.UpdateElements(ctx, message, []core.CardElementUpdate{{ElementID: "gateway_quick_resume", Element: element}})
}
