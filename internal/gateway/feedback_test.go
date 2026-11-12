package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type feedbackFixture struct {
	f       *fixture
	room    core.Room
	session string
	output  string
	card    core.Args
}

func newFeedbackFixture(t *testing.T, enabled bool, perMinute int) *feedbackFixture {
	t.Helper()
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Feedback.Enabled = enabled
		if perMinute > 0 {
			c.Limits.MessagesPerMinute = perMinute
		}
	})
	r := f.room()
	session, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	message := core.Message{ID: "origin-message", ChatID: r.ChatID, ChatType: "group", SenderID: "member", Text: "question text"}
	if _, err := f.s.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES(?,?,?,'done',?,?)", r.Key, message.ID, jsonText(message), time.Now().UnixMilli(), session.ID); err != nil {
		t.Fatal(err)
	}
	card := replyCard("回复", "answer text", nil, 0, true, replyDetails{}, time.Second, true)
	output, err := f.g.saveOutput(f.s.DB, r, session.ID, "answer text", true, "feedback-card", "", "", card, []core.Origin{{ID: message.ID, SenderID: message.SenderID}}, "", "", "reply", "done")
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.g.output(output)
	if err != nil {
		t.Fatal(err)
	}
	delivered := f.g.deliveredOutputCard(o, r)
	if err := f.g.bindQuickControls("feedback-card", delivered); err != nil {
		t.Fatal(err)
	}
	return &feedbackFixture{f: f, room: r, session: session.ID, output: output, card: delivered}
}

func (x *feedbackFixture) token(t *testing.T, action string) string {
	t.Helper()
	revision := x.f.g.quickRevision(x.room, x.session, "", x.output, "copy")
	return x.f.g.signQuick(quickControl{Room: x.room.Key, Session: x.session, Output: x.output, Revision: revision, Action: action})
}

func (x *feedbackFixture) click(t *testing.T, user, action string) string {
	t.Helper()
	text, err := x.f.g.Action(context.Background(), core.CardAction{UserID: user, ChatID: x.room.ChatID, MessageID: "feedback-card", Value: core.Args{"quick_control": x.token(t, action)}})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func (x *feedbackFixture) feedback(t *testing.T) (value, user, question, answer, nativeSession string, count int) {
	t.Helper()
	if err := x.f.s.DB.QueryRow("SELECT count(*) FROM feedback WHERE output=?", x.output).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		return "", "", "", "", "", 0
	}
	if err := x.f.s.DB.QueryRow("SELECT value,user,question,answer_summary,COALESCE(native_session,'') FROM feedback WHERE output=? ORDER BY at DESC LIMIT 1", x.output).Scan(&value, &user, &question, &answer, &nativeSession); err != nil {
		t.Fatal(err)
	}
	return value, user, question, answer, nativeSession, count
}

func TestFeedbackButtonsOnlyOnDoneReplyCard(t *testing.T) {
	done := newFeedbackFixture(t, true, 0)
	text := jsonText(done.card)
	if !strings.Contains(text, "gateway_quick_fb_yes") || !strings.Contains(text, "gateway_quick_fb_no") || !strings.Contains(text, "有用") || !strings.Contains(text, "无用") {
		t.Fatalf("done card missing feedback buttons: %s", text)
	}
	disabled := newFeedbackFixture(t, false, 0)
	if text := jsonText(disabled.card); strings.Contains(text, "gateway_quick_fb") {
		t.Fatalf("disabled project received feedback buttons: %s", text)
	}
	for name, card := range map[string]core.Args{
		"failed":       replyCard("执行失败", "answer", nil, 0, true, replyDetails{}, time.Second, false),
		"fragment":     replyCard("后台续轮已结束", "answer", nil, 0, true, replyDetails{}, time.Second, true),
		"streaming":    replyCard("处理中", "answer", nil, 0, false, replyDetails{}, time.Second, false),
		"file pending": replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true),
	} {
		o, err := done.f.g.output(done.output)
		if err != nil {
			t.Fatal(err)
		}
		o.Card = card
		rendered := done.f.g.outputCard(o, done.room, name == "file pending")
		if name != "file pending" {
			rendered = done.f.g.deliveredOutputCard(o, done.room)
		}
		if strings.Contains(jsonText(rendered), "gateway_quick_fb") {
			t.Fatalf("%s card received feedback buttons", name)
		}
	}
}

func TestFeedbackDoneStatusUsesSharedCardText(t *testing.T) {
	x := newFeedbackFixture(t, true, 0)
	label, _ := replyStatus("回复", 0, true, true)
	if label != replyDoneText {
		t.Fatalf("done label = %q, want %q", label, replyDoneText)
	}
	status, _ := replyCardStatus(t, x.card)
	if status != replyStatusContent(replyDoneText) {
		t.Fatalf("delivered status = %q", status)
	}
	if !strings.Contains(jsonText(x.card), "gateway_quick_fb") {
		t.Fatal("done status lost feedback controls")
	}
	fragment := replyCard("后台续轮已结束", "answer", nil, 0, true, replyDetails{}, time.Second, true)
	if replyCardDone(fragment) {
		t.Fatal("fragment status treated as a completed reply")
	}
}

func TestFeedbackRecordedAuditContainsOnlyMetadata(t *testing.T) {
	x := newFeedbackFixture(t, true, 0)
	if got := x.click(t, "member", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("feedback toast = %q", got)
	}
	var detail string
	if err := x.f.s.DB.QueryRow("SELECT detail FROM audit WHERE room=? AND event='feedback_recorded'", x.room.Key).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["session"] != x.session || fields["output"] != x.output || fields["value"] != "useful" {
		t.Fatalf("feedback audit fields = %#v", fields)
	}
	for _, forbidden := range []string{"question", "answer", "answerSummary", "user", "open_id", "message"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("feedback audit leaked %s: %#v", forbidden, fields)
		}
	}
}

func TestFeedbackClickAuthorizationBindingAndRate(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	if got := x.click(t, "stranger", "feedback_useful"); got != "" {
		t.Fatalf("unauthorized feedback toast = %q", got)
	}
	if _, _, _, _, _, count := x.feedback(t); count != 0 {
		t.Fatal("unauthorized click stored feedback")
	}
	if got := x.click(t, "member", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("useful toast = %q", got)
	}
	if value, user, question, answer, _, count := x.feedback(t); count != 1 || value != "useful" || user != "member" || question != "question text" || answer != "answer text" {
		t.Fatalf("stored feedback = %q %q %q %q %d", value, user, question, answer, count)
	}
	if got := x.click(t, "member", "feedback_useless"); got != "你已反馈过：有用（每人只能反馈一次）" {
		t.Fatalf("duplicate vote toast = %q", got)
	}
	if value, _, _, _, _, count := x.feedback(t); count != 1 || value != "useful" {
		t.Fatalf("duplicate vote changed feedback = %q %d", value, count)
	}
	if got := x.click(t, "second", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("second user toast = %q", got)
	}
	if _, _, _, _, _, count := x.feedback(t); count != 2 {
		t.Fatalf("per-user votes = %d", count)
	}
	rate := newFeedbackFixture(t, true, 1)
	if got := rate.click(t, "member", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("first rate click = %q", got)
	}
	if got := rate.click(t, "member", "feedback_useless"); !strings.Contains(got, "频繁") {
		t.Fatalf("rate limit toast = %q", got)
	}
	if value, _, _, _, _, _ := rate.feedback(t); value != "useful" {
		t.Fatalf("rate-limited click changed vote: %q", value)
	}
}

func TestFeedbackBindingAndSessionSwitchInvalidate(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	if _, err := x.f.s.DB.Exec("UPDATE card_controls SET revision='stale' WHERE message='feedback-card'"); err != nil {
		t.Fatal(err)
	}
	if got := x.click(t, "member", "feedback_useful"); !strings.Contains(got, "过期") {
		t.Fatalf("stale card toast = %q", got)
	}
	if _, _, _, _, _, count := x.feedback(t); count != 0 {
		t.Fatal("stale card stored feedback")
	}
	y := newFeedbackFixture(t, true, 100)
	if _, err := y.f.s.DB.Exec("UPDATE sessions SET state='archived' WHERE id=?", y.session); err != nil {
		t.Fatal(err)
	}
	if got := y.click(t, "member", "feedback_useful"); !strings.Contains(got, "过期") {
		t.Fatalf("switched session toast = %q", got)
	}
	if _, _, _, _, _, count := y.feedback(t); count != 0 {
		t.Fatal("switched session stored feedback")
	}
}

func TestFeedbackSnapshotTruncatesRedactsAndRecordsNativeSession(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	x.f.g.secrets = append(x.f.g.secrets, "secret-token")
	longQuestion := strings.Repeat("q", 5000) + " secret-token"
	longAnswer := strings.Repeat("a", 3000) + " secret-token"
	if _, err := x.f.s.DB.Exec("UPDATE inbox SET message=? WHERE room=? AND id='origin-message'", jsonText(core.Message{ID: "origin-message", ChatID: x.room.ChatID, ChatType: "group", SenderID: "member", Text: longQuestion}), x.room.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.s.DB.Exec("UPDATE outbox SET text=? WHERE id=?", longAnswer, x.output); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.s.DB.Exec("UPDATE sessions SET cc='native-session-1' WHERE id=?", x.session); err != nil {
		t.Fatal(err)
	}
	if got := x.click(t, "member", "feedback_useless"); got != "已记录：无用" {
		t.Fatalf("toast = %q", got)
	}
	_, _, question, answer, nativeSession, _ := x.feedback(t)
	if strings.Contains(question, "secret-token") || strings.Contains(answer, "secret-token") {
		t.Fatal("feedback snapshot was not redacted")
	}
	if len([]rune(question)) != 4000 || len([]rune(answer)) != 2000 {
		t.Fatalf("truncated lengths = %d/%d", len([]rune(question)), len([]rune(answer)))
	}
	if nativeSession != "native-session-1" {
		t.Fatalf("native session = %q", nativeSession)
	}
}

func feedbackElementLabel(update core.CardElementUpdate) string {
	text, _ := update.Element["text"].(map[string]any)
	value, _ := text["content"].(string)
	return value
}

func feedbackUpdates(t *testing.T, f *fixture) []core.CardElementUpdate {
	t.Helper()
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	return append([]core.CardElementUpdate(nil), f.tr.elementUpdates...)
}

func TestFeedbackCountsRefreshPerCard(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	if text := jsonText(x.card); !strings.Contains(text, "有用") || strings.Contains(text, "有用 · ") {
		t.Fatalf("initial feedback labels = %s", text)
	}
	if got := x.click(t, "member", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("first toast = %q", got)
	}
	eventually(t, func() bool { return len(feedbackUpdates(t, x.f)) >= 2 })
	updates := feedbackUpdates(t, x.f)
	if labels := []string{feedbackElementLabel(updates[len(updates)-2]), feedbackElementLabel(updates[len(updates)-1])}; labels[0] != "有用 · 1" || labels[1] != "无用" {
		t.Fatalf("first refresh labels = %#v", labels)
	}
	if got := x.click(t, "member", "feedback_useless"); got != "你已反馈过：有用（每人只能反馈一次）" {
		t.Fatalf("duplicate toast = %q", got)
	}
	if value, _, _, _, _, count := x.feedback(t); count != 1 || value != "useful" {
		t.Fatalf("duplicate changed feedback = %q %d", value, count)
	}
	before := len(feedbackUpdates(t, x.f))
	if got := x.click(t, "second", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("second user toast = %q", got)
	}
	eventually(t, func() bool { return len(feedbackUpdates(t, x.f)) > before })
	updates = feedbackUpdates(t, x.f)
	if got := feedbackElementLabel(updates[len(updates)-2]); got != "有用 · 2" {
		t.Fatalf("second refresh label = %q", got)
	}
	o, err := x.f.g.output(x.output)
	if err != nil {
		t.Fatal(err)
	}
	if final := jsonText(x.f.g.deliveredOutputCard(o, x.room)); !strings.Contains(final, "有用 · 2") || strings.Contains(final, "有用 · 1") {
		t.Fatalf("final full card does not match feedback counts: %s", final)
	}
}

func TestFeedbackCountRefreshFailureDoesNotLoseRecord(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	x.f.tr.elementUpdateHook = func(context.Context, string, []core.CardElementUpdate) error {
		return errors.New("card update unavailable")
	}
	if got := x.click(t, "member", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("toast = %q", got)
	}
	eventually(t, func() bool {
		var n int
		_ = x.f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='feedback_count_refresh_failed'").Scan(&n)
		return n == 1
	})
	if _, _, _, _, _, count := x.feedback(t); count != 1 {
		t.Fatal("refresh failure lost the feedback record")
	}
}

func TestFeedbackCountRefreshSkipsNonCardKitMessage(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	x.f.tr.elementUpdateHook = func(context.Context, string, []core.CardElementUpdate) error {
		return core.ErrCardNotEntity
	}
	if got := x.click(t, "member", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("toast = %q", got)
	}
	eventually(t, func() bool {
		var n int
		_ = x.f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='feedback_count_refresh_skipped'").Scan(&n)
		return n == 1
	})
	var detail string
	if err := x.f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='feedback_count_refresh_skipped'").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal([]byte(detail), &fields)
	if fields["reason"] != "not_cardkit" {
		t.Fatalf("skip audit = %#v", fields)
	}
}

func TestFeedbackConcurrentUsersConvergeCount(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	users := []string{"member", "second", "u3", "u4", "u5", "u6"}
	x.f.g.config.Groups[0].Members = append(x.f.g.config.Groups[0].Members, "u3", "u4", "u5", "u6")
	revision := x.f.g.quickRevision(x.room, x.session, "", x.output, "copy")
	var wg sync.WaitGroup
	for _, user := range users {
		user := user
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := x.f.g.signQuick(quickControl{Room: x.room.Key, Session: x.session, Output: x.output, Revision: revision, Action: "feedback_useful"})
			if _, err := x.f.g.Action(context.Background(), core.CardAction{UserID: user, ChatID: x.room.ChatID, MessageID: "feedback-card", Value: core.Args{"quick_control": token}}); err != nil {
				t.Errorf("feedback click for %s: %v", user, err)
			}
		}()
	}
	wg.Wait()
	if _, _, _, _, _, count := x.feedback(t); count != len(users) {
		t.Fatalf("concurrent feedback count = %d", count)
	}
	eventually(t, func() bool { return len(feedbackUpdates(t, x.f)) >= 2*len(users) })
	updates := feedbackUpdates(t, x.f)
	if got := feedbackElementLabel(updates[len(updates)-2]); got != "有用 · 6" {
		t.Fatalf("concurrent final label = %q", got)
	}
	o, err := x.f.g.output(x.output)
	if err != nil {
		t.Fatal(err)
	}
	if final := jsonText(x.f.g.deliveredOutputCard(o, x.room)); !strings.Contains(final, "有用 · 6") {
		t.Fatalf("full card after concurrent feedback = %s", final)
	}
}

func TestFeedbackMissingOriginLeavesQuestionEmpty(t *testing.T) {
	x := newFeedbackFixture(t, true, 100)
	if _, err := x.f.s.DB.Exec("DELETE FROM inbox WHERE room=? AND id='origin-message'", x.room.Key); err != nil {
		t.Fatal(err)
	}
	if got := x.click(t, "member", "feedback_useful"); got != "已记录：有用" {
		t.Fatalf("toast = %q", got)
	}
	_, _, question, _, _, _ := x.feedback(t)
	if question != "" {
		t.Fatalf("missing origin question = %q", question)
	}
}
