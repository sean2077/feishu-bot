package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func historyOutputRow(t *testing.T, f *fixture, id, room, target, text, kind, state string) {
	t.Helper()
	session, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(`INSERT INTO outbox(id,room,session,text,target,success,state,at,kind,file_target)
VALUES(?,?,?,?,?,1,?,?,?,?)`, id, room, session.ID, text, nullString(target), state, time.Now().UnixMilli(), kind, nil); err != nil {
		t.Fatal(err)
	}
}

func historyBotMessage(f *fixture, id, text string) core.Message {
	m := f.message(id, text, "")
	m.SenderIsBot = true
	m.SenderIsSelf = true
	return m
}

func TestHistoryBackfillsOnlyDeliveredReplyCards(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	done := historyBotMessage(f, "card-done", core.HistoryCardPlaceholder)
	empty := historyBotMessage(f, "card-empty", "")
	readable := historyBotMessage(f, "card-readable", "already readable")
	file := historyBotMessage(f, "card-file", core.HistoryCardPlaceholder)
	pending := historyBotMessage(f, "card-pending", core.HistoryCardPlaceholder)
	otherRoom := historyBotMessage(f, "card-other-room", core.HistoryCardPlaceholder)
	status := historyBotMessage(f, "card-status", core.HistoryCardPlaceholder)
	uncertain := historyBotMessage(f, "card-uncertain", core.HistoryCardPlaceholder)
	missing := historyBotMessage(f, "card-missing", core.HistoryCardPlaceholder)
	human := f.message("human", core.HistoryCardPlaceholder, "member")
	f.tr.historyPage = core.HistoryPage{Messages: []core.Message{done, empty, readable, file, pending, otherRoom, status, uncertain, missing, human}}

	historyOutputRow(t, f, "out-done", r.Key, "card-done", "delivered answer", "reply", "done")
	historyOutputRow(t, f, "out-empty", r.Key, "card-empty", "empty card answer", "reply", "done")
	historyOutputRow(t, f, "out-readable", r.Key, "card-readable", "should not replace", "reply", "done")
	historyOutputRow(t, f, "out-file", r.Key, "", "file answer", "reply", "done")
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_target='card-file' WHERE id='out-file'"); err != nil {
		t.Fatal(err)
	}
	historyOutputRow(t, f, "out-pending", r.Key, "card-pending", "not delivered", "reply", "pending")
	historyOutputRow(t, f, "out-other", r.Key+"-other", "card-other-room", "other room", "reply", "done")
	historyOutputRow(t, f, "out-status", r.Key, "card-status", "status answer", "status", "done")
	historyOutputRow(t, f, "out-uncertain", r.Key, "card-uncertain", "uncertain answer", "reply", "uncertain")

	page, err := f.g.history(context.Background(), r, "session-a", core.Args{"limit": 20})
	if err != nil {
		t.Fatal(err)
	}
	messages := page["messages"].([]core.Message)
	byID := map[string]string{}
	for _, m := range messages {
		byID[m.ID] = m.Text
	}
	want := map[string]string{
		"card-done":       "delivered answer",
		"card-empty":      "empty card answer",
		"card-readable":   "already readable",
		"card-file":       core.HistoryCardPlaceholder,
		"card-pending":    core.HistoryCardPlaceholder,
		"card-other-room": core.HistoryCardPlaceholder,
		"card-status":     core.HistoryCardPlaceholder,
		"card-uncertain":  core.HistoryCardPlaceholder,
		"card-missing":    core.HistoryCardPlaceholder,
		"human":           core.HistoryCardPlaceholder,
	}
	for id, expected := range want {
		if byID[id] != expected {
			t.Fatalf("history %s = %q, want %q", id, byID[id], expected)
		}
	}
}
