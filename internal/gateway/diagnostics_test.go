package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
)

func auditDetail(t *testing.T, f *fixture, event string) map[string]any {
	t.Helper()
	var raw string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event=? ORDER BY seq DESC LIMIT 1", event).Scan(&raw); err != nil {
		t.Fatalf("audit %s: %v", event, err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func TestTurnResultTimingDiagnostics(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("diag", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "accepted", UUID: d.UUID})
	w.emit(core.WorkerEvent{Type: "activity", Phase: "tool_start", ToolUseID: "tool-1", Tool: "Bash"})
	time.Sleep(10 * time.Millisecond)
	w.emit(core.WorkerEvent{Type: "activity", Phase: "tool_end", ToolUseID: "tool-1"})
	w.emit(core.WorkerEvent{Type: "text", Text: "hi", Partial: true, TurnID: d.UUID, BlockID: "block-1"})
	w.emit(core.WorkerEvent{Type: "result", Success: true, UUID: d.UUID, UUIDs: []string{d.UUID}, ResultID: "result-1", Text: "hi"})
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='turn_result'").Scan(&n) == nil && n > 0
	})
	detail := auditDetail(t, f, "turn_result")
	if detail["turn"] != d.UUID {
		t.Fatalf("turn = %v, want %s", detail["turn"], d.UUID)
	}
	for _, key := range []string{"queueMs", "startupMs", "sendMs", "firstMs", "toolsMs"} {
		if _, ok := detail[key]; !ok {
			t.Fatalf("turn_result missing %s: %#v", key, detail)
		}
	}
	if detail["reused"] != false {
		t.Fatalf("reused = %v, want false", detail["reused"])
	}
	if tools, _ := detail["toolsMs"].(float64); tools < 5 {
		t.Fatalf("toolsMs = %v, want the measured tool duration", detail["toolsMs"])
	}
	if send, _ := detail["sendMs"].(float64); send < 0 {
		t.Fatalf("sendMs = %v", detail["sendMs"])
	}
	if first, _ := detail["firstMs"].(float64); first < 0 {
		t.Fatalf("firstMs = %v", detail["firstMs"])
	}
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='turn_delivery' AND json_extract(detail,'$.turn')=?", d.UUID).Scan(&n) == nil && n > 0
	})
	delivery := auditDetail(t, f, "turn_delivery")
	if delivery["turn"] != d.UUID || delivery["session"] != f.openSession(f.room().Key).ID || delivery["attempt"].(float64) != 1 {
		t.Fatalf("turn_delivery correlation = %#v", delivery)
	}
	if delivery["outboxState"] != "done" {
		t.Fatalf("turn_delivery state = %v", delivery["outboxState"])
	}
	if _, ok := delivery["totalMs"]; !ok {
		t.Fatalf("delivered turn missing totalMs: %#v", delivery)
	}
}

func TestTurnDeliveryAttemptsAndTotal(t *testing.T) {
	f := newFixture(t, nil)
	session, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	card := replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true)
	id, err := f.g.saveOutput(f.s.DB, f.room(), session.ID, "answer", true, "", "", "", card, nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	f.g.rememberTurnDelivery(id, &turnDeliveryReport{turn: "turn-1", session: session.ID, startedAt: time.Now().Add(-time.Second)})
	f.tr.updateHook = func(context.Context, string, core.Args) error {
		return classifiedFailure{retryable: false}
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); ok || err == nil {
		t.Fatalf("blocked delivery = %t %v", ok, err)
	}
	first := auditDetail(t, f, "turn_delivery")
	if first["attempt"].(float64) != 1 || first["outboxState"] != "blocked" {
		t.Fatalf("first attempt = %#v", first)
	}
	if _, ok := first["totalMs"]; ok {
		t.Fatalf("blocked attempt invented totalMs: %#v", first)
	}
	failure := auditDetail(t, f, "output_delivery_failed")
	if failure["phase"] != "delivery" || failure["reason"] != "other" {
		t.Fatalf("delivery failure diagnostics = %#v", failure)
	}
	f.tr.updateHook = nil
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), true); !ok || err != nil {
		t.Fatalf("explicit retry = %t %v", ok, err)
	}
	var rows *sql.Rows
	rows, err = f.s.DB.Query("SELECT detail FROM audit WHERE event='turn_delivery' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if len(details) != 2 {
		t.Fatalf("attempt audits = %#v", details)
	}
	last := details[1]
	if last["attempt"].(float64) != 2 || last["outboxState"] != "done" || last["turn"] != "turn-1" {
		t.Fatalf("retry audit = %#v", last)
	}
	if _, ok := last["totalMs"]; !ok {
		t.Fatalf("done retry missing totalMs: %#v", last)
	}
}

func TestTurnDeliveryWithoutReportOmitsTotal(t *testing.T) {
	f := newFixture(t, nil)
	session, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	card := replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true)
	id, err := f.g.saveOutput(f.s.DB, f.room(), session.ID, "answer", true, "", "", "", card, nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); !ok || err != nil {
		t.Fatalf("delivery = %t %v", ok, err)
	}
	detail := auditDetail(t, f, "turn_delivery")
	if detail["outboxState"] != "done" {
		t.Fatalf("state = %v", detail["outboxState"])
	}
	if _, ok := detail["totalMs"]; ok {
		t.Fatalf("delivery without turn report invented totalMs: %#v", detail)
	}
}

func TestDispatchFailureAuditIncludesPhaseAndReason(t *testing.T) {
	f := newFixture(t, nil)
	f.g.factory = func(context.Context, core.Room, core.Session, core.Workspace, func(core.WorkerEvent)) (core.Worker, error) {
		return nil, errors.New("project settings file is invalid")
	}
	f.send(f.message("dispatch-fail", "hello", "member"))
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='dispatch_failed'").Scan(&n) == nil && n > 0
	})
	detail := auditDetail(t, f, "dispatch_failed")
	if detail["phase"] != "launch" || detail["reason"] != "settings_file" {
		t.Fatalf("dispatch diagnostics = %#v", detail)
	}
}

func TestWorkerFailureAuditIncludesPhaseAndReason(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("worker-fail", "hello", "member"))
	w := f.worker()
	w.emit(core.WorkerEvent{Type: "error", Message: "Claude native initialization timed out"})
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='worker_failure'").Scan(&n) == nil && n > 0
	})
	detail := auditDetail(t, f, "worker_failure")
	if detail["phase"] != "initialize" || detail["reason"] != "timeout" {
		t.Fatalf("worker diagnostics = %#v", detail)
	}
}

func TestWorkerStopDiagnosticsUseHostIntent(t *testing.T) {
	w := &fakeWorker{emit: func(core.WorkerEvent) {}}
	if err := stopWorkerReason(context.Background(), w, "idle_release"); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.stopReason.Load().(string); got != "idle_release" {
		t.Fatalf("stop reason = %q", got)
	}
	setWorkerTurn(w, true)
	if !w.turnActive.Load() {
		t.Fatal("turn active was not recorded")
	}
}

func TestFailureReasonsAreFixedTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"settings", errors.New("project settings file is invalid"), "settings_file"},
		{"identity", store.ErrLaunchIdentityChanged, "launch_identity"},
		{"process start", errors.New("start Claude native executable: access denied"), "process_start"},
		{"unconfirmed", process.ErrUnconfirmed, "process_unconfirmed"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"profile", config.ErrModelProfileUnavailable, "model_profile"},
		{"other", errors.New("unexpected failure"), "other"},
	} {
		if got := failureReason(tc.err); got != tc.want {
			t.Fatalf("%s reason = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := failurePhase(&dispatchFailure{phase: "send", reason: "other", err: errors.New("x")}); got != "send" {
		t.Fatalf("wrapped phase = %q", got)
	}
	if got := failurePhase(errors.New("x")); got != "launch" {
		t.Fatalf("default phase = %q", got)
	}
}
