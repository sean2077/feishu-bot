package gateway

import (
	"context"
	"github.com/sean2077/feishu-bot/internal/core"
	"strconv"
)

func (g *Gateway) sessionsCard(r core.Room, actor string, text string) (core.Args, string, error) {
	page, err := g.sessionPage(r, "")
	if err != nil {
		return nil, "", err
	}
	return g.sessionPageCard(r, actor, text, page), page.Current, nil
}

func (g *Gateway) sessionPageCard(r core.Room, actor, text string, page sessionListPage) core.Args {
	if page.Current == "" {
		return nil
	}
	ids := []string{r.Key, page.Current, "session-list", page.Query, page.NextCursor}
	for _, session := range page.Items {
		ids = append(ids, session.ID)
	}
	revision := core.Hash(jsonText(ids))[:32]
	controls := []any{}
	add := func(label string, control quickControl) core.Args {
		control.Room, control.Session, control.Revision = r.Key, page.Current, revision
		b := button(label, nil, "default")
		delete(b, "value")
		b["element_id"] = "gateway_quick_r" + strconv.Itoa(len(controls))
		b["behaviors"] = []any{core.Args{"type": "callback", "value": core.Args{"quick_control": g.signQuick(control)}}}
		controls = append(controls, b)
		return b
	}
	if canManage(r, actor) {
		for _, session := range page.Items {
			if session.State == "open" || len(controls) >= 5 {
				continue
			}
			b := add("恢复 "+cut(session.DisplayName(), 60), quickControl{TargetSession: session.ID, Action: "resume"})
			b["confirm"] = core.Args{"title": core.Args{"tag": "plain_text", "content": "选择历史会话"}, "text": core.Args{"tag": "plain_text", "content": "将停止当前运行并取消未交付的输入，选择此历史会话。下一条消息才恢复其上下文。"}}
		}
	}
	if page.NextCursor != "" {
		add("下一页", quickControl{Action: "sessions", Cursor: page.NextCursor})
	}
	if page.Total > 0 {
		add("刷新搜索", quickControl{Action: "sessions", Query: page.Query})
	}
	if len(controls) == 0 {
		return nil
	}
	return DisplayCard("本聊天会话", core.Redact(text, g.secrets), controls...)
}

func (g *Gateway) sessionsReply(ctx context.Context, r core.Room, m core.Message) error {
	_, arg := splitCommand(m.Text)
	page, err := g.sessionPage(r, arg)
	if err != nil {
		return g.notice(ctx, r, err.Error(), &m)
	}
	text := sessionPageText(page, canManage(r, m.SenderID))
	if page.JSON {
		return g.notice(ctx, r, text, &m)
	}
	card := g.sessionPageCard(r, m.SenderID, text, page)
	if card == nil {
		return g.notice(ctx, r, text, &m)
	}
	id, err := provider(ctx, func() (string, error) { return g.transport.Card(ctx, r, card, messageReply(m), m.ThreadID) })
	if err != nil {
		return err
	}
	if err := g.store.RecordOutput(id, r.Key, page.Current); err != nil {
		return err
	}
	return g.bindQuickControls(id, card)
}
