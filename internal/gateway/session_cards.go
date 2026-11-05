package gateway

import (
	"context"
	"strconv"

	"github.com/sean2077/feishu-bot/internal/core"
)

func (g *Gateway) sessionsCard(r core.Room, actor string, text string) (core.Args, string, error) {
	if !canManage(r, actor) {
		return nil, "", nil
	}
	current, err := g.currentSession(r)
	if err != nil || current == "" {
		return nil, current, err
	}
	list, err := g.store.ListSessions(r.Key)
	if err != nil {
		return nil, "", err
	}
	archived := []core.Session{}
	ids := []string{r.Key, current, "session-list"}
	for _, session := range list {
		if session.State != "open" && len(archived) < 5 {
			archived = append(archived, session)
			ids = append(ids, session.ID)
		}
	}
	if len(archived) == 0 {
		return nil, current, nil
	}
	revision := core.Hash(jsonText(ids))[:32]
	controls := []any{}
	for i, session := range archived {
		p, err := g.store.Preferences(r.Key, session.ID)
		if err != nil {
			return nil, "", err
		}
		label := p.Name
		if label == "" {
			label = cut(session.ID, 8)
		}
		control := quickControl{Room: r.Key, Session: current, TargetSession: session.ID, Revision: revision, Action: "resume"}
		b := button("恢复 "+cut(label, 60), nil, "default")
		delete(b, "value")
		b["element_id"] = "gateway_quick_r" + strconv.Itoa(i)
		b["behaviors"] = []any{core.Args{"type": "callback", "value": core.Args{"quick_control": g.signQuick(control)}}}
		b["confirm"] = core.Args{"title": core.Args{"tag": "plain_text", "content": "选择历史会话"}, "text": core.Args{"tag": "plain_text", "content": "将停止当前运行并取消未交付的输入，选择此历史会话。下一条消息才恢复其上下文。"}}
		controls = append(controls, b)
	}
	return DisplayCard("本聊天会话", core.Redact(text, g.secrets), controls...), current, nil
}

func (g *Gateway) sessionsReply(ctx context.Context, r core.Room, m core.Message) error {
	text, _, err := g.management("/sessions", "", r, m.SenderID)
	if err != nil {
		return err
	}
	card, session, err := g.sessionsCard(r, m.SenderID, text)
	if err != nil {
		return err
	}
	if card == nil {
		return g.notice(ctx, r, text, &m)
	}
	id, err := provider(ctx, func() (string, error) { return g.transport.Card(ctx, r, card, messageReply(m), m.ThreadID) })
	if err != nil {
		return err
	}
	if err := g.store.RecordOutput(id, r.Key, session); err != nil {
		return err
	}
	return g.bindQuickControls(id, card)
}
