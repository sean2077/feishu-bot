package native

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func taintedWorker(t *testing.T) (*fixture, *worker) {
	t.Helper()
	f := newFixture(t)
	f.start()
	w := f.worker.(*worker)
	w.SetTaintCheck(func() bool { return true })
	return f, w
}

func hookResult(t *testing.T, w *worker, name string, input args) args {
	t.Helper()
	done := make(chan args, 1)
	go func() {
		result, err := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": name, "tool_input": input})
		if err != nil {
			t.Errorf("hook error: %v", err)
		}
		done <- result
	}()
	select {
	case result := <-done:
		return result
	case <-time.After(8 * time.Second):
		t.Fatal("hook did not return")
		return nil
	}
}

func taintDenied(result args) bool {
	return object(result["hookSpecificOutput"])["permissionDecision"] == "deny"
}

func TestTaintedHookApprovesAndDeniesWrites(t *testing.T) {
	f, w := taintedWorker(t)
	done := make(chan args, 1)
	go func() {
		result, _ := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": args{"command": "echo fixture"}})
		done <- result
	}()
	request := f.wait(func(e core.WorkerEvent) bool { return e.Type == "request" && e.Tainted })
	if request.Name != "Bash" || request.Kind != "permission" {
		t.Fatalf("tainted request = %+v", request)
	}
	if err := w.Respond(context.Background(), request.ID, args{"approved": true}); err != nil {
		t.Fatal(err)
	}
	if result := <-done; taintDenied(result) {
		t.Fatalf("approved tainted write was denied: %#v", result)
	}

	go func() {
		result, _ := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Write", "tool_input": args{"file_path": filepath.Join(f.workspace.Work, "denied.txt")}})
		done <- result
	}()
	request = f.wait(func(e core.WorkerEvent) bool { return e.Type == "request" && e.Tainted && e.Name == "Write" })
	if err := w.Respond(context.Background(), request.ID, args{"approved": false}); err != nil {
		t.Fatal(err)
	}
	if result := <-done; !taintDenied(result) {
		t.Fatalf("denied tainted write was allowed: %#v", result)
	}
}

func TestTaintedHookAllowsReadOnlyAndGatewayTools(t *testing.T) {
	f, w := taintedWorker(t)
	file := filepath.Join(f.workspace.Work, "readable.txt")
	if err := os.WriteFile(file, []byte("text"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		input args
	}{
		{"Read", args{"file_path": file}},
		{"Grep", args{"pattern": "text", "path": f.workspace.Work}},
		{"Glob", args{"pattern": "*.txt", "path": f.workspace.Work}},
		{"TodoWrite", args{}},
		{"ToolSearch", args{}},
		{"TaskOutput", args{}},
		{"TaskList", args{}},
		{"TaskGet", args{}},
		{"Agent", args{}},
		{"mcp__gateway__send_file", args{"path": "x.txt"}},
		{"mcp__gateway__project_action", args{"action": "read_status"}},
	} {
		result := hookResult(t, w, tc.name, tc.input)
		if taintDenied(result) {
			t.Fatalf("read-only tool %s was denied: %#v", tc.name, result)
		}
	}
}

func TestTaintedHookFailsClosedWhenWorkerStops(t *testing.T) {
	_, w := taintedWorker(t)
	done := make(chan args, 1)
	go func() {
		result, _ := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": args{"command": "echo fixture"}})
		done <- result
	}()
	select {
	case <-done:
		t.Fatal("tainted write completed without approval")
	case <-time.After(200 * time.Millisecond):
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := w.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if !taintDenied(result) {
			t.Fatalf("stopped worker allowed tainted write: %#v", result)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("tainted hook did not fail closed after Stop")
	}
}

func TestTaintedHookAllowsAgentButChildWriteStillPrompts(t *testing.T) {
	f, w := taintedWorker(t)
	if result := hookResult(t, w, "Agent", args{}); taintDenied(result) {
		t.Fatal("Agent was denied in a tainted turn")
	}
	// Subagent tool calls traverse this same hook with the same worker taint
	// state; a child write must still prompt.
	go func() {
		_, _ = w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Write", "tool_input": args{"file_path": "child.txt"}})
	}()
	request := f.wait(func(e core.WorkerEvent) bool { return e.Type == "request" })
	if !request.Tainted || request.Name != "Write" {
		t.Fatalf("child write event = %+v", request)
	}
	_ = w.Respond(context.Background(), request.ID, args{"approved": false})
}

func TestInitializationToolHookOutlivesApprovalWait(t *testing.T) {
	init := initialization(core.Project{}, 1000, "", nil)
	hooks := object(init["hooks"])
	for _, raw := range array(hooks["PreToolUse"]) {
		hook := object(raw)
		ids, _ := hook["hookCallbackIds"].([]string)
		if len(ids) == 1 && ids[0] == "tool" {
			timeout, ok := count(hook["timeout"])
			if !ok || timeout < 31 {
				t.Fatalf("tool hook timeout = %#v", hook["timeout"])
			}
			return
		}
	}
	t.Fatal("tool hook was not registered")
}
