package gateway

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
)

func TestActionApprovalExecutesTheApprovedFileSnapshot(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish a file", Executable: "fixture", Args: []string{"{{file}}"}, Parameters: map[string]core.Parameter{"file": {MaxLength: 200, WorkspaceFile: true}}}}
	})
	seen := make(chan string, 1)
	f.g.actionRunner = func(_ context.Context, executable string, args []string, _ string, _ map[string]string, _ int64) (int, string, error) {
		b, e := os.ReadFile(args[0])
		seen <- string(b)
		return 0, string(b), e
	}
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	work := s.workspace.Work
	s.mu.Unlock()
	path := filepath.Join(work, "document.txt")
	if e := os.WriteFile(path, []byte("approved original"), 0600); e != nil {
		t.Fatal(e)
	}
	w.emit(core.WorkerEvent{Type: "request", ID: "publish-request", Kind: "action", Name: "publish", Input: core.Args{"file": "document.txt"}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	if !strings.Contains(jsonText(card.card), "SHA256") {
		t.Fatal("approval missing content digest")
	}
	if e := os.WriteFile(path, []byte("changed after approval prompt"), 0600); e != nil {
		t.Fatal(e)
	}
	_, e := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: cardValue(card.card, "批准一次")})
	if e != nil {
		t.Fatal(e)
	}
	a := response(t, w)
	if !boolArg(a, "ok") {
		t.Fatal("approved action failed", a)
	}
	select {
	case content := <-seen:
		if content != "approved original" {
			t.Fatal("executed mutable source instead of approved snapshot", content)
		}
	case <-time.After(time.Second):
		t.Fatal("action did not execute")
	}
	var state string
	if e = f.s.DB.QueryRow("SELECT state FROM action_runs").Scan(&state); e != nil || state != "done" {
		t.Fatal("action result not committed", state, e)
	}
}
func TestUnconfirmedActionCleanupBlocksNewSessionAndClose(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Runtime.MaxWorkers = 1
		c.Projects[0].Actions = []core.ActionSpec{{ID: "check", Executable: "fixture", ReadOnly: true}}
	})
	f.g.actionRunner = func(context.Context, string, []string, string, map[string]string, int64) (int, string, error) {
		return 0, "", errors.New("operation cancelled; process termination unconfirmed")
	}
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	saved, e := f.s.Session(f.room().Key)
	if e != nil {
		t.Fatal(e)
	}
	w.emit(core.WorkerEvent{Type: "request", ID: "unsafe-action", Kind: "action", Name: "check", Input: core.Args{}})
	a := response(t, w)
	if boolArg(a, "ok") {
		t.Fatal("uncertain action reported success")
	}
	eventually(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		for _, a := range f.g.actions {
			if a.unconfirmed {
				return true
			}
		}
		return false
	})
	if e = f.g.Message(context.Background(), f.message("rotate", "/new", "owner")); e == nil {
		t.Fatal("new session allowed with unknown action process")
	}
	current, e := f.s.Session(f.room().Key)
	if e != nil || current.ID != saved.ID {
		t.Fatal("session rotated", current, e)
	}
	m := f.message("other-input", "work", "member")
	m.ChatID = "other"
	f.send(m)
	if f.starts.Load() != 1 {
		t.Fatal("unknown action did not retain worker capacity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = f.g.Close(ctx); e == nil || !strings.Contains(e.Error(), "action process termination") {
		t.Fatal("close released service lock with unknown action process", e)
	}
	// The simulated process has no OS resources. Clear only its injected fence
	// so the fixture can perform normal final database cleanup.
	f.g.mu.Lock()
	f.g.actions = map[string]*actionExecution{}
	f.g.mu.Unlock()
	for _, s := range f.g.allSlots() {
		s.mu.Lock()
		s.stopUnconfirmed = false
		s.mu.Unlock()
	}
}
func TestApprovalPersistenceFailureDeniesAndReleasesWaiter(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "request", Kind: "permission", Name: "Bash", Input: core.Args{}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	if _, e := f.s.DB.Exec("CREATE TRIGGER approval_storage_failure BEFORE UPDATE ON requests BEGIN SELECT RAISE(ABORT,'injected write failure'); END"); e != nil {
		t.Fatal(e)
	}
	_, e := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: cardValue(card.card, "批准一次")})
	if e != nil {
		t.Fatal(e)
	}
	a := response(t, w)
	if boolArg(a, "approved") {
		t.Fatal("unstored approval authorized an operation")
	}
	f.g.approvalMu.Lock()
	remaining := len(f.g.approvals)
	f.g.approvalMu.Unlock()
	if remaining != 0 {
		t.Fatal("failed approval retained local waiter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = f.g.Close(ctx); e != nil {
		t.Fatal("storage failure prevented shutdown", e)
	}
}
func TestConfiguredHeaderAndActionSecretsRedactedInCardsAndAudit(t *testing.T) {
	t.Setenv("FIXTURE_ACTION_SECRET", `action"secret`)
	f := newFixture(t, func(c *core.Config) {
		c.Model.ExtraHeaders = map[string]string{"x-private": `header"secret`}
		c.Projects[0].Actions = []core.ActionSpec{{ID: "fixture", Executable: "fixture", Env: map[string]string{"PRIVATE": "FIXTURE_ACTION_SECRET"}}}
	})
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	payload := core.Args{"values": []any{`action"secret`, `header"secret`}}
	w.emit(core.WorkerEvent{Type: "request", ID: "request", Kind: "permission", Name: "Bash", Input: payload})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := jsonText(f.tr.cardLast().card)
	if strings.Contains(card, "action") || strings.Contains(card, "header\\") || !strings.Contains(card, "[REDACTED]") {
		t.Fatal("configured secret exposed by approval", card)
	}
	f.g.audit(f.room().Key, "redaction-fixture", payload)
	var raw string
	if e := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='redaction-fixture'").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(raw, "action") || strings.Contains(raw, "header") || !strings.Contains(raw, "[REDACTED]") {
		t.Fatal("configured secret exposed in audit", raw)
	}
}
func TestActionProcessFixture(t *testing.T) {
	mode := os.Getenv("FEISHU_ACTION_TEST_PROCESS")
	if mode == "" {
		return
	}
	switch mode {
	case "success":
		_, _ = os.Stdout.WriteString("action output")
		os.Exit(0)
	case "exit":
		os.Exit(17)
	case "overflow":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 1024*1024+1))
		os.Exit(0)
	case "timeout":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "descendant":
		cmd := exec.Command(os.Args[0], "-test.run=^TestActionProcessFixture$")
		cmd.Env = envList(cleanActionEnv(map[string]string{"FEISHU_ACTION_TEST_PROCESS": "heartbeat", "FEISHU_ACTION_HEARTBEAT": os.Getenv("FEISHU_ACTION_HEARTBEAT"), "GORACE": "atexit_sleep_ms=0"}))
		if err := cmd.Start(); err != nil {
			os.Exit(20)
		}
		for i := 0; i < 100; i++ {
			if data, _ := os.ReadFile(os.Getenv("FEISHU_ACTION_HEARTBEAT")); len(data) > 0 {
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(21)
	case "heartbeat":
		f, err := os.OpenFile(os.Getenv("FEISHU_ACTION_HEARTBEAT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(22)
		}
		// Bound the helper itself so a broken ownership regression cannot
		// leave a test process running indefinitely.
		for i := 0; i < 300; i++ {
			_, _ = f.WriteString(".")
			time.Sleep(10 * time.Millisecond)
		}
		_ = f.Close()
		os.Exit(0)
	default:
		os.Exit(19)
	}
}
func TestActionRunnerBoundsRealChildOutputAndTime(t *testing.T) {
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"success", "exit", "overflow", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			env := cleanActionEnv(map[string]string{"FEISHU_ACTION_TEST_PROCESS": mode, "GORACE": "atexit_sleep_ms=0"})
			timeout := int64(1000)
			if mode == "timeout" {
				timeout = 30
			}
			code, out, e := runActionCommand(context.Background(), executable, []string{"-test.run=^TestActionProcessFixture$"}, "", env, timeout)
			switch mode {
			case "success":
				if e != nil || code != 0 || out != "action output" {
					t.Fatal(code, out, e)
				}
			case "exit":
				if e != nil || code != 17 {
					t.Fatal(code, e)
				}
			case "overflow":
				if e == nil || !strings.Contains(e.Error(), "output size limit") || len(out) != 1024*1024 {
					t.Fatal("output overflow not bounded", len(out), e)
				}
			case "timeout":
				if e == nil || !strings.Contains(e.Error(), "timed out") {
					t.Fatal("timeout not enforced", e)
				}
			}
		})
	}
}

func TestUnconfirmedFactoryFailureRetainsSlotAndServiceLock(t *testing.T) {
	f := newFixture(t, nil)
	entered := make(chan struct{})
	f.g.factory = func(context.Context, core.Room, core.Session, core.Workspace, func(core.WorkerEvent)) (core.Worker, error) {
		close(entered)
		return nil, process.ErrUnconfirmed
	}
	f.send(f.message("one", "task", "member"))
	<-entered
	eventually(t, func() bool {
		s := f.g.getSlot(f.room().Key)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopUnconfirmed
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := f.g.Close(ctx); e == nil {
		t.Fatal("unconfirmed failed startup released service lock")
	}
	for _, s := range f.g.allSlots() {
		s.mu.Lock()
		s.stopUnconfirmed = false
		s.mu.Unlock()
	}
}
func TestEarlyNativeControlWaitsForFactoryHandle(t *testing.T) {
	f := newFixture(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	f.g.factory = func(ctx context.Context, r core.Room, s core.Session, workspace core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		w := &fakeWorker{emit: emit, sent: make(chan core.Delivery, 100), responses: make(chan core.Args, 100)}
		f.started <- w
		emit(core.WorkerEvent{Type: "request", ID: "early-history", Kind: "history", Input: core.Args{}})
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return w, nil
	}
	f.send(f.message("one", "task", "member"))
	<-entered
	w := f.worker()
	select {
	case a := <-w.responses:
		t.Fatal("request responded before factory published handle", a)
	default:
	}
	close(release)
	if a := response(t, w); stringArg(a, "requestID") != "early-history" {
		t.Fatal("early request lost", a)
	}
	_ = f.delivery(w)
}

func TestStopCannotClearUnknownConcurrentStartup(t *testing.T) {
	f := newFixture(t, nil)
	entered := make(chan struct{})
	f.g.factory = func(ctx context.Context, _ core.Room, _ core.Session, _ core.Workspace, _ func(core.WorkerEvent)) (core.Worker, error) {
		close(entered)
		<-ctx.Done()
		return nil, process.ErrUnconfirmed
	}
	f.send(f.message("one", "task", "member"))
	<-entered
	before, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.g.Message(ctx, f.message("new", "/new", "owner")); err == nil {
		t.Fatal("new session ignored startup cleanup failure")
	}
	s := f.g.getSlot(f.room().Key)
	if s == nil {
		t.Fatal("uncertain startup released its reserved slot")
	}
	after, err := f.s.Session(f.room().Key)
	if err != nil || after.ID != before.ID {
		t.Fatal("session rotated after uncertain startup", after.ID, before.ID, err)
	}
	s.mu.Lock()
	unconfirmed := s.stopUnconfirmed
	s.stopUnconfirmed = false // The fixture creates no actual process.
	s.mu.Unlock()
	if !unconfirmed {
		t.Fatal("stop cleared unconfirmed startup ownership")
	}
}

func TestActionCompletionCleansDescendantsWithClosedStdio(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	env := cleanActionEnv(map[string]string{"FEISHU_ACTION_TEST_PROCESS": "descendant", "FEISHU_ACTION_HEARTBEAT": heartbeat, "GORACE": "atexit_sleep_ms=0"})
	code, _, err := runActionCommand(context.Background(), executable, []string{"-test.run=^TestActionProcessFixture$"}, "", env, 3000)
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil || len(before) == 0 {
		t.Fatal("descendant did not start", err)
	}
	time.Sleep(75 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil || len(after) != len(before) {
		t.Fatal("action reported completion while descendant still ran", len(before), len(after), err)
	}
}
