package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func queuedMessages(t *testing.T, f *fixture, room string) []core.Message {
	t.Helper()
	var messages []core.Message
	rows, err := f.s.DB.Query("SELECT message FROM inbox WHERE room=? AND status!='uncertain'", room)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m core.Message
		if json.Unmarshal([]byte(raw), &m) != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestResumeInterruptedRecoveryCard(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	session, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	original := core.Message{ID: "card:abc", ChatID: r.ChatID, ChatType: "group", SenderID: "member", Text: "original question", Time: time.Now().UnixMilli(), InteractionMessageID: "om_card"}
	if _, err = f.s.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES(?,?,?,'uncertain',?,?)", r.Key, original.ID, jsonText(original), original.Time, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("INSERT INTO outbox(id,room,session,text,reply_to,success,state,at,kind,origins) VALUES(?,?,?,?,?,?,?,?,?,?)", "status-out", r.Key, session.ID, "已停止", original.ID, 0, "done", original.Time, "status", "[]"); err != nil {
		t.Fatal(err)
	}
	card := f.g.deliveredOutputCard(outgoing{Kind: "recovery", ID: "recovery-out", Session: session.ID, State: "done", Text: "上次运行中断"}, r)
	if !strings.Contains(jsonText(card), "gateway_quick_resume") {
		t.Fatalf("recovery card lacks resume button: %s", jsonText(card))
	}
	statusCard := f.g.outputCard(outgoing{Kind: "status", ID: "status-out", Session: session.ID, State: "done", Text: "已停止"}, r, false)
	if !strings.Contains(jsonText(statusCard), "gateway_quick_resume") {
		t.Fatalf("status card lacks resume button: %s", jsonText(statusCard))
	}
	if err = f.g.bindQuickControls("recovery-card", card); err != nil {
		t.Fatal(err)
	}
	value := cardValue(card, "继续")
	if value == nil {
		t.Fatal("resume button has no callback value")
	}
	action := core.CardAction{UserID: "second", ChatID: r.ChatID, MessageID: "recovery-card", Value: value}
	if text, err := f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "只有原提问人") {
		t.Fatalf("unauthorized resume = %q %v", text, err)
	}
	action.UserID = "member"
	text, err := f.g.Action(context.Background(), action)
	if err != nil || !strings.Contains(text, "已继续") {
		t.Fatalf("resume = %q %v", text, err)
	}
	if text, err = f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "已继续或卡片已失效") {
		t.Fatalf("duplicate resume = %q %v", text, err)
	}
	found := false
	for _, message := range queuedMessages(t, f, r.Key) {
		if strings.Contains(message.Text, "original question") {
			found = true
			if message.InteractionMessageID != "recovery-card" {
				t.Fatalf("resume reply target = %q", message.InteractionMessageID)
			}
		}
	}
	if !found {
		t.Fatalf("resume prompt was not queued: %#v", queuedMessages(t, f, r.Key))
	}
	var uncertain int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE room=? AND status='uncertain'", r.Key).Scan(&uncertain); err != nil || uncertain != 1 {
		t.Fatalf("uncertain input changed: %d %v", uncertain, err)
	}
}

func TestResumeButtonVisibleWhileStopInProgress(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	session, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	request := core.Message{ID: "request", ChatID: r.ChatID, ChatType: "group", SenderID: "member", Text: "request text", Time: time.Now().UnixMilli()}
	if _, err = f.s.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES(?,?,?,'cancelled',?,?)", r.Key, request.ID, jsonText(request), request.Time, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("INSERT INTO outbox(id,room,session,text,reply_to,success,state,at,kind,origins) VALUES(?,?,?,?,?,?,?,?,?,?)", "status-blocked", r.Key, session.ID, "已停止", request.ID, 0, "done", request.Time, "status", "[]"); err != nil {
		t.Fatal(err)
	}
	f.g.mu.Lock()
	f.g.stopBlocked[r.Key] = true
	f.g.mu.Unlock()
	defer func() {
		f.g.mu.Lock()
		delete(f.g.stopBlocked, r.Key)
		f.g.mu.Unlock()
	}()
	card := f.g.outputCard(outgoing{Kind: "status", ID: "status-blocked", Session: session.ID, State: "done"}, r, false)
	if !strings.Contains(jsonText(card), "gateway_quick_resume") {
		t.Fatalf("stop-in-progress card lacks resume button: %s", jsonText(card))
	}
	if err = f.g.bindQuickControls("blocked-card", card); err != nil {
		t.Fatal(err)
	}
	value := cardValue(card, "继续")
	if value == nil {
		t.Fatal("resume button has no callback value")
	}
	action := core.CardAction{UserID: "member", ChatID: r.ChatID, MessageID: "blocked-card", Value: value}
	if text, err := f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "暂停或停止未确认") {
		t.Fatalf("blocked resume = %q %v", text, err)
	}
}

func TestResumeStatusCardFollowsResumedTurn(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	session, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	resumed := core.Message{ID: "resume-input", ChatID: r.ChatID, ChatType: "group", SenderID: "member", Text: "上次回答被中断。原始请求正文", Time: time.Now().UnixMilli(), InteractionMessageID: "first-card"}
	if _, err = f.s.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES(?,?,?,'cancelled',?,?)", r.Key, resumed.ID, jsonText(resumed), resumed.Time, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("INSERT INTO outbox(id,room,session,text,reply_to,success,state,at,kind,origins) VALUES(?,?,?,?,?,?,?,?,?,?)", "status-second", r.Key, session.ID, "以下为中断前输出", "first-card", 0, "done", resumed.Time, "status", "[]"); err != nil {
		t.Fatal(err)
	}
	card := f.g.outputCard(outgoing{Kind: "status", ID: "status-second", Session: session.ID, State: "done"}, r, false)
	if !strings.Contains(jsonText(card), "gateway_quick_resume") {
		t.Fatalf("second-generation status card lacks resume button: %s", jsonText(card))
	}
	if err = f.g.bindQuickControls("second-card", card); err != nil {
		t.Fatal(err)
	}
	value := cardValue(card, "继续")
	if value == nil {
		t.Fatal("resume button has no callback value")
	}
	action := core.CardAction{UserID: "member", ChatID: r.ChatID, MessageID: "second-card", Value: value}
	if text, err := f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "已继续") {
		t.Fatalf("second-generation resume = %q %v", text, err)
	}
	for _, message := range queuedMessages(t, f, r.Key) {
		if strings.Contains(message.Text, "请只继续完成这条原始请求") && strings.Contains(message.Text, "原始请求正文") {
			if message.InteractionMessageID != "second-card" {
				t.Fatalf("resume reply target = %q", message.InteractionMessageID)
			}
			return
		}
	}
	t.Fatalf("second-generation resume prompt was not queued: %#v", queuedMessages(t, f, r.Key))
}

func TestResumeStatusCardUsesInterruptedRequest(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	session, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	old := core.Message{ID: "old-uncertain", ChatID: r.ChatID, ChatType: "group", SenderID: "member", Text: "一个更早的无关请求", Time: now + 1000}
	if _, err = f.s.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES(?,?,?,'uncertain',?,?)", r.Key, old.ID, jsonText(old), old.Time, session.ID); err != nil {
		t.Fatal(err)
	}
	interrupted := core.Message{ID: "interrupted-input", ChatID: r.ChatID, ChatType: "group", SenderID: "member", Text: "请写一篇 Go 并发模型简介", Time: now}
	if _, err = f.s.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES(?,?,?,'cancelled',?,?)", r.Key, interrupted.ID, jsonText(interrupted), interrupted.Time, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("INSERT INTO outbox(id,room,session,text,reply_to,success,state,at,kind,origins) VALUES(?,?,?,?,?,?,?,?,?,?)", "status-source", r.Key, session.ID, "以下为中断前输出", interrupted.ID, 0, "done", now, "status", "[]"); err != nil {
		t.Fatal(err)
	}
	card := f.g.outputCard(outgoing{Kind: "status", ID: "status-source", Session: session.ID, State: "done", Text: "以下为中断前输出"}, r, false)
	if !strings.Contains(jsonText(card), "gateway_quick_resume") {
		t.Fatalf("status card lacks resume button: %s", jsonText(card))
	}
	if err = f.g.bindQuickControls("status-card", card); err != nil {
		t.Fatal(err)
	}
	value := cardValue(card, "继续")
	if value == nil {
		t.Fatal("resume button has no callback value")
	}
	action := core.CardAction{UserID: "member", ChatID: r.ChatID, MessageID: "status-card", Value: value}
	if text, err := f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "已继续") {
		t.Fatalf("resume = %q %v", text, err)
	}
	var got core.Message
	found := false
	for _, message := range queuedMessages(t, f, r.Key) {
		if strings.Contains(message.Text, "Go 并发模型简介") {
			found = true
			got = message
		}
		if strings.Contains(message.Text, "一个更早的无关请求") {
			t.Fatalf("resume used an unrelated uncertain request: %q", message.Text)
		}
	}
	if !found {
		t.Fatalf("anchored resume prompt was not queued: %#v", queuedMessages(t, f, r.Key))
	}
	if got.InteractionMessageID != "status-card" {
		t.Fatalf("resume reply target = %q", got.InteractionMessageID)
	}
}
