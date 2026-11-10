package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func (g *Gateway) feedbackEnabled(r core.Room) bool {
	p, ok := g.config.Project(r.ProjectID)
	return ok && p.Feedback.Enabled
}

// feedbackControls are appended only to the final, delivered reply card. The
// tokens share the quick-control revision and card binding. CardKit limits
// element_id to 20 characters; longer IDs are truncated server-side and
// collide, which the API reports as code 300301 (duplicate element_id).
func (g *Gateway) feedbackControls(r core.Room, session, output, revision string) []any {
	if session == "" || output == "" || !g.feedbackEnabled(r) {
		return nil
	}
	makeButton := func(action, label, elementID string) core.Args {
		p := quickControl{Room: r.Key, Session: session, Output: output, Revision: revision, Action: action}
		b := button(label, nil, "default")
		delete(b, "value")
		b["element_id"] = elementID
		b["behaviors"] = []any{core.Args{"type": "callback", "value": core.Args{"quick_control": g.signQuick(p)}}}
		return b
	}
	columns := []any{
		core.Args{"tag": "column", "width": "auto", "elements": []any{makeButton("feedback_useful", "有用", "gateway_quick_fb_yes")}},
		core.Args{"tag": "column", "width": "auto", "elements": []any{makeButton("feedback_useless", "无用", "gateway_quick_fb_no")}},
	}
	return []any{
		core.Args{"tag": "hr", "element_id": "gateway_quick_fb_div"},
		core.Args{"tag": "column_set", "element_id": "gateway_quick_fb", "flex_mode": "none", "horizontal_spacing": "medium", "columns": columns},
	}
}

// replyCardDone matches the visibly completed status row only. Intermediate,
// failed, cancelled and file-pending cards keep their other status text. The
// expected content comes from cards.go so a wording change cannot silently
// drop the feedback controls.
func replyCardDone(card core.Args) bool {
	elements, _ := argsOf(card["body"])["elements"].([]any)
	for i, value := range elements {
		element := argsOf(value)
		if stringArg(element, "element_id") != "reply_status" && !(i == 0 && legacyReplyStatusRow(element)) {
			continue
		}
		columns, _ := element["columns"].([]any)
		if len(columns) != 2 {
			continue
		}
		right, _ := argsOf(columns[1])["elements"].([]any)
		if len(right) != 1 {
			continue
		}
		return stringArg(argsOf(argsOf(right[0])["text"]), "content") == replyStatusContent(replyDoneText)
	}
	return false
}

func (g *Gateway) recordFeedback(ctx context.Context, r core.Room, p quickControl, a core.CardAction, value string) (string, error) {
	project, ok := g.config.Project(r.ProjectID)
	if !ok || !project.Feedback.Enabled {
		return "反馈功能未启用。", nil
	}
	if p.Output == "" || a.MessageID == "" {
		return "交付记录不存在或已失效。", nil
	}
	o, err := g.outputContext(ctx, p.Output)
	if err != nil || o.Room != r.Key || o.Session != p.Session || o.Kind != "reply" || !o.Success || o.State != "done" {
		return "交付记录不存在或已失效。", nil
	}
	question := cut(core.Redact(g.feedbackQuestion(ctx, r.Key, o.Origins), g.secrets), 4000)
	answer := cut(core.Redact(o.Text, g.secrets), 2000)
	var native sql.NullString
	_ = g.store.DB.QueryRowContext(ctx, "SELECT cc FROM sessions WHERE id=?", o.Session).Scan(&native)
	_, err = g.store.DB.ExecContext(ctx, `INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(output,user) DO UPDATE SET value=excluded.value,question=excluded.question,answer_summary=excluded.answer_summary,native_session=excluded.native_session,message=excluded.message,at=excluded.at`,
		core.NewID(), r.Key, r.ProjectID, o.Session, nullString(native.String), o.ID, a.MessageID, a.UserID, value, question, answer, time.Now().UnixMilli())
	if err != nil {
		g.audit(r.Key, "feedback_store_failed", core.Args{"output": o.ID, "error": core.Redact(err.Error(), g.secrets)})
		return "反馈记录失败，请稍后再试。", nil
	}
	g.audit(r.Key, "feedback_recorded", core.Args{"session": o.Session, "output": o.ID, "value": value})
	if value == "useful" {
		return "已记录：有用", nil
	}
	return "已记录：无用", nil
}

// feedbackQuestion reads the triggering inbox body if it has not been cleared
// by retention. Missing or cleaned origins leave the question empty.
func (g *Gateway) feedbackQuestion(ctx context.Context, room string, origins []core.Origin) string {
	for _, origin := range origins {
		var raw string
		if err := g.store.DB.QueryRowContext(ctx, "SELECT message FROM inbox WHERE room=? AND id=?", room, origin.ID).Scan(&raw); err != nil {
			continue
		}
		var message core.Message
		if json.Unmarshal([]byte(raw), &message) == nil && strings.TrimSpace(message.Text) != "" {
			return message.Text
		}
	}
	return ""
}
