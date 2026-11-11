package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestTaintSurvivesBackgroundAndClearsOnNextInput(t *testing.T) {
	s := &slot{}
	s.markTainted(historyAccess{Target: "target"})
	if !s.taintCheck() {
		t.Fatal("cross-chat read did not taint the turn")
	}
	s.mu.Lock()
	s.background = 2
	s.newHumanInputLocked()
	s.mu.Unlock()
	if !s.taintCheck() || !s.taintPendingClear {
		t.Fatal("taint cleared while background work was still running")
	}
	s.mu.Lock()
	s.background = 0
	if s.taintPendingClear {
		s.clearTaintLocked()
	}
	s.mu.Unlock()
	if s.taintCheck() || s.taintPendingClear {
		t.Fatal("taint did not clear after background work ended")
	}

	s.markTainted(historyAccess{Target: "target"})
	s.mu.Lock()
	s.newHumanInputLocked()
	s.mu.Unlock()
	if s.taintCheck() {
		t.Fatal("taint did not clear on the next human input")
	}
}

func TestTaintedSendFileRequiresApproval(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	slot := f.g.getSlot(f.room().Key)
	if err := os.WriteFile(filepath.Join(slot.workspace.Work, "out.txt"), []byte("tainted-file"), 0600); err != nil {
		t.Fatal(err)
	}
	slot.markTainted(historyAccess{Target: "target"})
	done := make(chan struct {
		result core.Args
		err    error
	}, 1)
	go func() {
		result, err := f.g.request(slot, core.WorkerEvent{Type: "request", Kind: "send_file", Input: core.Args{"path": "out.txt"}})
		done <- struct {
			result core.Args
			err    error
		}{result, err}
	}()
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	value := cardValue(card.card, "批准一次")
	if value == nil {
		t.Fatal("tainted send_file card missing approval button")
	}
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("approval toast = %q", got)
	}
	select {
	case out := <-done:
		if out.err != nil || !boolArg(out.result, "ok") {
			t.Fatalf("tainted send_file = %#v %v", out.result, out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tainted send_file did not finish")
	}
}

func TestTaintedSendFileDenialDoesNotWriteOutput(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	slot := f.g.getSlot(f.room().Key)
	if err := os.WriteFile(filepath.Join(slot.workspace.Work, "denied.txt"), []byte("tainted-file"), 0600); err != nil {
		t.Fatal(err)
	}
	slot.markTainted(historyAccess{Target: "target"})
	done := make(chan core.Args, 1)
	go func() {
		result, _ := f.g.request(slot, core.WorkerEvent{Type: "request", Kind: "send_file", Input: core.Args{"path": "denied.txt"}})
		done <- result
	}()
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	deny := cardValue(card.card, "拒绝")
	if deny == nil {
		t.Fatal("tainted send_file card missing deny button")
	}
	waitApprovalBinding(t, f, card, stringArg(deny, "request"))
	_ = clickApproval(t, f, card, deny, "owner")
	select {
	case result := <-done:
		if boolArg(result, "ok") {
			t.Fatalf("denied tainted send_file succeeded: %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tainted send_file denial did not settle")
	}
}

func TestTaintedApprovalAuditsPromptWithoutContent(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	policy := taintedApprovalPolicy(approvalPolicyForTool(core.Project{Maintainers: []string{"owner"}}, nil), "owner", []string{"target", "other"})
	go func() {
		_, err := f.g.ask(ctx, f.room(), "session-a", "Bash", core.Args{"command": "fixture-command"}, "tainted summary", nil, policy)
		done <- err
	}()
	eventually(t, func() bool {
		var n int
		_ = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='tainted_turn_prompt'").Scan(&n)
		return n == 1
	})
	var raw string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='tainted_turn_prompt'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["tool"] != "Bash" || detail["session"] != "session-a" || detail["request"] == "" {
		t.Fatalf("tainted audit = %#v", detail)
	}
	targets, _ := detail["targets"].([]any)
	if len(targets) != 2 || targets[0] != "target" || targets[1] != "other" {
		t.Fatalf("tainted targets = %#v", detail["targets"])
	}
	if _, ok := detail["input"]; ok {
		t.Fatalf("tainted audit leaked tool input: %#v", detail)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tainted approval did not settle after cancellation")
	}
}
