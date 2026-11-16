package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestStatusJSONIncludesRoomScopedPendingUncertain(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	other, _ := RoomFor(f.c, "other", "")
	for _, row := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO inbox(room,id,message,status,at) VALUES(?,?, '{}','uncertain',100)`, []any{r.Key, "om_here"}},
		{`INSERT INTO inbox(room,id,message,status,at) VALUES(?,?, '{}','uncertain',200)`, []any{other.Key, "om_elsewhere"}},
		{`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-uncertain',?,?,'',0,'uncertain',300)`, []any{r.Key, "s"}},
		{`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-blocked',?,?,'',0,'blocked',400)`, []any{r.Key, "s"}},
		{`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-elsewhere',?,?,'',0,'uncertain',500)`, []any{other.Key, "s"}},
	} {
		if _, err := f.s.DB.Exec(row.query, row.args...); err != nil {
			t.Fatal(err)
		}
	}
	text, handled, err := f.g.management("/status", "--json", r, "member")
	if err != nil || !handled {
		t.Fatalf("status = handled %t err %v", handled, err)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		t.Fatal(err)
	}
	pending, ok := view["pendingUncertain"].(map[string]any)
	if !ok {
		t.Fatalf("pendingUncertain missing: %s", text)
	}
	inbox, _ := pending["inbox"].(map[string]any)
	outbox, _ := pending["outbox"].(map[string]any)
	if inbox["count"] != float64(1) || outbox["count"] != float64(2) {
		t.Fatalf("pendingUncertain counts = %#v", pending)
	}
	rooms, _ := inbox["rooms"].([]any)
	if len(rooms) != 1 || rooms[0] != r.Key {
		t.Fatalf("pendingUncertain leaked another room: %#v", inbox["rooms"])
	}
	if pending["inbox"].(map[string]any)["samples"] == nil || pending["outbox"].(map[string]any)["samples"] == nil {
		t.Fatalf("pendingUncertain missing samples: %#v", pending)
	}
	plain, _, err := f.g.management("/status", "", r, "member")
	if err != nil {
		t.Fatal(err)
	}
	if containsJSONMarker(plain) {
		t.Fatalf("plain /status gained JSON diagnostics: %s", plain)
	}
}

func containsJSONMarker(text string) bool {
	return len(text) >= 2 && (text[0] == '{' || text[0] == '[')
}

func TestAcknowledgedRecordsAreNotPendingOrRetried(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	if _, err := f.s.DB.Exec(`INSERT INTO inbox(room,id,message,status,at) VALUES(?, 'om_ack','{}','acknowledged',100)`, r.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-ack',?,?,'',0,'acknowledged',100)`, r.Key, "s"); err != nil {
		t.Fatal(err)
	}
	text, _, err := f.g.management("/status", "--json", r, "member")
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(text), &view); err != nil {
		t.Fatal(err)
	}
	if view["pendingOutputs"] != float64(0) {
		t.Fatalf("acknowledged output counted as pending: %s", text)
	}
	inputs, _ := view["inputs"].(map[string]any)
	if inputs["uncertain"] != float64(0) {
		t.Fatalf("acknowledged input counted as uncertain: %s", text)
	}
	states, _ := view["outboxStates"].(map[string]any)
	if states["acknowledged"] != float64(1) {
		t.Fatalf("acknowledged state not observed: %s", text)
	}
	outputs, _, err := f.g.management("/outputs", "--json", r, "member")
	if err != nil {
		t.Fatal(err)
	}
	var items []any
	if err := json.Unmarshal([]byte(outputs), &items); err != nil || len(items) != 0 {
		t.Fatalf("/outputs listed acknowledged output: %s (%v)", outputs, err)
	}
	if ids := f.g.retryable(r.Key); len(ids) != 0 {
		t.Fatalf("retry scan selected acknowledged output: %#v", ids)
	}
	if sent, err := f.g.deliverOutput(context.Background(), "out-ack", r, true); err != nil || sent {
		t.Fatalf("explicit retry of acknowledged output = %t %v", sent, err)
	}
	record := store.AdoptedTopic{Room: r.Key}
	if reason, err := f.g.adoptedRoomBusy(record); err != nil || reason != "" {
		t.Fatalf("acknowledged output blocked re-attach: %q %v", reason, err)
	}
}

func TestAcknowledgedFileDoesNotDeferReplyDelivery(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	origins := []core.Origin{{ID: "om_turn", SenderID: "member"}}
	reply, err := f.g.saveOutput(f.s.DB, r, s.ID, "answer", true, "", "", "", nil, origins, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.g.saveOutput(f.s.DB, r, s.ID, "", true, "", "", "", nil, origins, "report.md", "/tmp/report.md", "file", "acknowledged"); err != nil {
		t.Fatal(err)
	}
	output, err := f.g.output(reply)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := f.g.filesPendingFor(output)
	if err != nil || pending {
		t.Fatalf("acknowledged file deferred reply delivery: pending %t err %v", pending, err)
	}
}
