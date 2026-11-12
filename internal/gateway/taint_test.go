package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestTaintNotClearedByInputInsertedIntoRunningTurn(t *testing.T) {
	s := &slot{deliveries: map[string]core.Delivery{}}
	s.markTainted(historyAccess{Target: "target"})
	s.mu.Lock()
	s.turn = &turnTiming{turn: "active"}
	s.deliveries["delivery"] = core.Delivery{}
	s.newTurnInputLocked()
	s.mu.Unlock()
	if !s.taintCheck() {
		t.Fatal("an input inserted into the running turn cleared taint")
	}
	s.mu.Lock()
	s.turn = nil
	delete(s.deliveries, "delivery")
	s.newTurnInputLocked()
	s.mu.Unlock()
	if s.taintCheck() {
		t.Fatal("the next turn did not clear taint")
	}
}

func TestTaintedActionKeepsOriginalApprovers(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish", Executable: "fixture", Approvers: []string{"owner"}}}
	})
	envs := make(chan map[string]string, 1)
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, env map[string]string, _ int64) (int, string, error) {
		envs <- env
		return 0, "", nil
	}
	room, ok := f.c.RoomFor("p2p_member", "member")
	if !ok {
		t.Fatal("missing member DM")
	}
	session, err := f.s.NewSession(room.Key)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := f.c.Project("project")
	requester := &core.RequestContext{ChatID: room.ChatID, ChatType: "p2p", SenderOpenID: "member", Senders: []string{"member"}, MessageIDs: []string{"m"}}
	done := make(chan core.Args, 1)
	go func() {
		result, _ := f.g.executeActionTainted(context.Background(), room, session.ID, project, "publish", core.Args{}, core.Workspace{}, requester, true, []string{"target"})
		done <- result
	}()
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	confirm := f.tr.cardLast()
	if confirm.room.DMUser != "member" {
		t.Fatalf("taint confirmation went to %#v", confirm.room)
	}
	if !strings.Contains(jsonText(confirm.card), "操作：Publish") {
		t.Fatalf("taint confirmation omitted the action description: %s", jsonText(confirm.card))
	}
	value := cardValue(confirm.card, "批准一次")
	waitApprovalBinding(t, f, confirm, stringArg(value, "request"))
	if got := clickApproval(t, f, confirm, value, "member"); got != "已批准一次" {
		t.Fatalf("taint confirmation = %q", got)
	}
	eventually(t, func() bool { return f.tr.cardCount() > 1 })
	approval := f.tr.cardLast()
	if approval.room.DMUser != "owner" {
		t.Fatalf("original approval went to %#v instead of the owner", approval.room)
	}
	approve := cardValue(approval.card, "批准一次")
	waitApprovalBinding(t, f, approval, stringArg(approve, "request"))
	if got := clickApproval(t, f, approval, approve, "member"); got == "已批准一次" {
		t.Fatal("DM user self-approved the action after only the taint confirmation")
	}
	if got := clickApproval(t, f, approval, approve, "owner"); got != "已批准一次" {
		t.Fatalf("owner action approval = %q", got)
	}
	select {
	case result := <-done:
		if !boolArg(result, "ok") {
			t.Fatalf("tainted action result = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tainted action did not finish")
	}
	env := <-envs
	if env["FEISHU_ACTION_APPROVER_OPEN_ID"] != "owner" || env["FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID"] != "member" {
		t.Fatalf("tainted action env = %#v", env)
	}
	var runRequest, runApprover, taintRequest string
	if err := f.s.DB.QueryRow("SELECT request,approver,taint_request FROM action_runs").Scan(&runRequest, &runApprover, &taintRequest); err != nil {
		t.Fatal(err)
	}
	if runRequest != stringArg(approve, "request") || runApprover != "owner" || taintRequest != stringArg(value, "request") {
		t.Fatalf("tainted action run metadata = %q %q %q", runRequest, runApprover, taintRequest)
	}
}

func TestTaintedActionKeepsForbidSelfApproval(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Owners = []string{"owner"}
		c.Projects[0].Approvals.ForbidSelfApproval = true
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish", Executable: "fixture", Approvers: []string{"member"}}}
	})
	fakeActionSuccess(f)
	room, ok := f.c.RoomFor("p2p_member", "member")
	if !ok {
		t.Fatal("missing member DM")
	}
	session, err := f.s.NewSession(room.Key)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := f.c.Project("project")
	requester := &core.RequestContext{ChatID: room.ChatID, ChatType: "p2p", SenderOpenID: "member", Senders: []string{"member"}, MessageIDs: []string{"m"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan core.Args, 1)
	go func() {
		result, _ := f.g.executeActionTainted(ctx, room, session.ID, project, "publish", core.Args{}, core.Workspace{}, requester, true, []string{"target"})
		done <- result
	}()
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	confirm := f.tr.cardLast()
	value := cardValue(confirm.card, "批准一次")
	waitApprovalBinding(t, f, confirm, stringArg(value, "request"))
	if got := clickApproval(t, f, confirm, value, "member"); got != "已批准一次" {
		t.Fatalf("taint confirmation = %q", got)
	}
	eventually(t, func() bool { return f.tr.cardCount() > 1 })
	approval := f.tr.cardLast()
	approve := cardValue(approval.card, "批准一次")
	waitApprovalBinding(t, f, approval, stringArg(approve, "request"))
	if got := clickApproval(t, f, approval, approve, "member"); got != selfApprovalToast {
		t.Fatalf("self approval after taint = %q", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tainted self-approval test did not settle")
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
