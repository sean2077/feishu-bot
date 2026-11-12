package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

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
	var messages []core.Message
	rows, err := f.s.DB.Query("SELECT message FROM inbox WHERE room=? AND status!='uncertain'", r.Key)
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
	found := false
	for _, message := range messages {
		if message.Text == resumeInterruptedPrompt {
			found = true
			if message.InteractionMessageID != "om_card" {
				t.Fatalf("resume reply target = %q", message.InteractionMessageID)
			}
		}
	}
	if !found {
		t.Fatalf("resume prompt was not queued: %#v", messages)
	}
	var uncertain int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE room=? AND status='uncertain'", r.Key).Scan(&uncertain); err != nil || uncertain != 1 {
		t.Fatalf("uncertain input changed: %d %v", uncertain, err)
	}
}
