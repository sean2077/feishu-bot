package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	counts, _ := g.feedbackCounts(context.Background(), output)
	columns := []any{
		core.Args{"tag": "column", "width": "auto", "elements": []any{g.feedbackButton(r, session, output, revision, "feedback_useful", "gateway_quick_fb_yes", feedbackLabel("有用", counts["useful"]))}},
		core.Args{"tag": "column", "width": "auto", "elements": []any{g.feedbackButton(r, session, output, revision, "feedback_useless", "gateway_quick_fb_no", feedbackLabel("无用", counts["useless"]))}},
	}
	return []any{
		core.Args{"tag": "hr", "element_id": "gateway_quick_fb_div"},
		core.Args{"tag": "column_set", "element_id": "gateway_quick_fb", "flex_mode": "none", "horizontal_spacing": "medium", "columns": columns},
	}
}

func (g *Gateway) feedbackButton(r core.Room, session, output, revision, action, elementID, label string) core.Args {
	p := quickControl{Room: r.Key, Session: session, Output: output, Revision: revision, Action: action}
	b := button(label, nil, "default")
	delete(b, "value")
	b["element_id"] = elementID
	b["behaviors"] = []any{core.Args{"type": "callback", "value": core.Args{"quick_control": g.signQuick(p)}}}
	return b
}

func feedbackLabel(base string, count int) string {
	if count > 0 {
		return fmt.Sprintf("%s · %d", base, count)
	}
	return base
}

func (g *Gateway) feedbackCounts(ctx context.Context, output string) (map[string]int, error) {
	counts := map[string]int{}
	rows, err := g.store.DB.QueryContext(ctx, "SELECT value,count(*) FROM feedback WHERE output=? GROUP BY value", output)
	if err != nil {
		return counts, err
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		var count int
		if err = rows.Scan(&value, &count); err != nil {
			return counts, err
		}
		counts[value] = count
	}
	return counts, rows.Err()
}

// refreshFeedbackCounts updates only the two button elements. A gateway-side
// keyed lock serializes refreshes per card and reads the counts immediately
// before sending, so concurrent clicks converge on the latest values even when
// an earlier refresh finishes later.
func (g *Gateway) refreshFeedbackCounts(ctx context.Context, r core.Room, p quickControl, messageID string) {
	updater, ok := g.transport.(core.CardElementUpdater)
	if !ok {
		g.audit(r.Key, "feedback_count_refresh_skipped", core.Args{"output": p.Output, "message": messageID, "reason": "unsupported_transport"})
		return
	}
	unlock := g.feedbackLocks.lock(messageID)
	defer unlock()
	counts, err := g.feedbackCounts(ctx, p.Output)
	if err != nil {
		g.audit(r.Key, "feedback_count_refresh_failed", core.Args{"output": p.Output, "message": messageID, "error": core.Redact(err.Error(), g.secrets)})
		return
	}
	updates := []core.CardElementUpdate{
		{ElementID: "gateway_quick_fb_yes", Element: g.feedbackButton(r, p.Session, p.Output, p.Revision, "feedback_useful", "gateway_quick_fb_yes", feedbackLabel("有用", counts["useful"]))},
		{ElementID: "gateway_quick_fb_no", Element: g.feedbackButton(r, p.Session, p.Output, p.Revision, "feedback_useless", "gateway_quick_fb_no", feedbackLabel("无用", counts["useless"]))},
	}
	if err = updater.UpdateElements(ctx, messageID, updates); err != nil {
		if errors.Is(err, core.ErrCardNotEntity) {
			g.audit(r.Key, "feedback_count_refresh_skipped", core.Args{"output": p.Output, "message": messageID, "reason": "not_cardkit"})
			return
		}
		g.audit(r.Key, "feedback_count_refresh_failed", core.Args{"output": p.Output, "message": messageID, "error": core.Redact(err.Error(), g.secrets)})
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
	result, err := g.store.DB.ExecContext(ctx, `INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(output,user) DO NOTHING`,
		core.NewID(), r.Key, r.ProjectID, o.Session, nullString(native.String), o.ID, a.MessageID, a.UserID, value, question, answer, time.Now().UnixMilli())
	if err != nil {
		g.audit(r.Key, "feedback_store_failed", core.Args{"output": o.ID, "error": core.Redact(err.Error(), g.secrets)})
		return "反馈记录失败，请稍后再试。", nil
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		g.audit(r.Key, "feedback_store_failed", core.Args{"output": o.ID, "error": core.Redact(err.Error(), g.secrets)})
		return "反馈记录失败，请稍后再试。", nil
	}
	if inserted == 0 {
		var existing string
		_ = g.store.DB.QueryRowContext(ctx, "SELECT value FROM feedback WHERE output=? AND user=?", o.ID, a.UserID).Scan(&existing)
		label := "已反馈过"
		if existing == "useful" {
			label = "有用"
		} else if existing == "useless" {
			label = "无用"
		}
		if existing == "" {
			return "你已反馈过（每人只能反馈一次）", nil
		}
		return "你已反馈过：" + label + "（每人只能反馈一次）", nil
	}
	g.audit(r.Key, "feedback_recorded", core.Args{"session": o.Session, "output": o.ID, "value": value})
	g.task(func() { g.refreshFeedbackCounts(g.ctx, r, p, a.MessageID) })
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
