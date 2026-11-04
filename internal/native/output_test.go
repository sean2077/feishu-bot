package native

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestBackgroundOriginsNeverBorrowOrCompleteCurrentHumanInput(t *testing.T) {
	known := map[string]bool{"first": true, "current": true}
	out := newOutput(func(id string) bool { return known[id] })
	out.handle(args{"type": "user", "uuid": "first", "origin": args{"kind": "human"}})
	out.handle(args{"type": "system", "subtype": "task_started", "task_id": "task", "run_id": "run"})
	out.end()
	delete(known, "first")
	out.handle(args{"type": "user", "uuid": "current", "origin": args{"kind": "human"}})
	notification := args{"type": "user", "origin": args{"kind": "task-notification", "runId": "run"}}
	out.handle(notification)
	message := args{"type": "assistant", "message": args{"id": "bg-msg", "content": []any{args{"type": "text", "text": "Background done"}}}}
	events := out.handle(message)
	if len(events) != 1 || events[0].Source != "background" || events[0].TurnID != "first" || events[0].BackgroundID != "run" {
		t.Fatal(events)
	}
	uuid, ids := resultOrigin(args{"type": "result"}, known, out.origin(args{}), out.eventSource(args{}))
	if uuid != "" || len(ids) > 0 {
		t.Fatalf("background completed human input: %s %v", uuid, ids)
	}
	out.handle(args{"type": "user", "origin": args{"kind": "task-notification", "runId": "unknown"}})
	if out.origin(args{}) != "" {
		t.Fatal("unknown background borrowed foreground anchor")
	}
	out.handle(args{"type": "user", "uuid": "current", "origin": args{"kind": "human"}})
	uuid, ids = resultOrigin(args{"type": "result", "user_message_uuid": "unknown"}, known, out.origin(args{}), out.eventSource(args{}))
	if uuid != "current" || len(ids) != 1 {
		t.Fatalf("retained human anchor not used: %s %v", uuid, ids)
	}
}
func TestNestedTaskOriginAndSubagentContent(t *testing.T) {
	known := map[string]bool{"human": true}
	out := newOutput(func(id string) bool { return known[id] })
	out.handle(args{"type": "user", "uuid": "human"})
	out.handle(args{"type": "system", "subtype": "task_started", "task_id": "parent"})
	out.end()
	out.handle(args{"type": "system", "subtype": "task_started", "parent_task_id": "parent", "task_id": "child"})
	out.handle(args{"type": "user", "origin": args{"kind": "task-notification", "senderTaskId": "child"}})
	if out.origin(args{}) != "human" {
		t.Fatal("nested task lost its human origin")
	}
	if got := out.handle(args{"type": "assistant", "parent_tool_use_id": "agent", "message": args{"content": []any{args{"type": "text", "text": "internal"}}}}); len(got) != 0 {
		t.Fatal("subagent output leaked", got)
	}
}
func TestContextUsesLastMainRequestAndUnambiguousNativeWindow(t *testing.T) {
	c := contextSnapshot{}
	c.observe(args{"type": "stream_event", "event": args{"type": "message_start", "message": args{"id": "main", "model": "fixture", "usage": args{"input_tokens": 40, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 10}}}})
	c.observe(args{"type": "assistant", "message": args{"id": "main", "model": "fixture", "usage": args{"input_tokens": 0}}})
	c.observe(args{"type": "assistant", "parent_tool_use_id": "agent", "message": args{"id": "child", "model": "child", "usage": args{"input_tokens": 99999}}})
	result := args{"modelUsage": args{"fixture": args{"contextWindow": 200000}, "fixture[1m]": args{"contextWindow": 321000}, "child": args{"contextWindow": 555555}}}
	snapshot := c.result(result, "fixture[1M]")
	if snapshot["inputTokens"] != int64(80) || snapshot["windowTokens"] != int64(321000) {
		t.Fatal(snapshot)
	}
	object(result["modelUsage"])["fixture[1M]"] = args{"contextWindow": 654000}
	if c.result(result, "fixture[1M]")["windowTokens"] != nil {
		t.Fatal("conflicting native windows were guessed")
	}
	c.observe(args{"type": "stream_event", "event": args{"type": "message_start", "message": args{"id": "invalid", "model": "fixture", "usage": args{"input_tokens": 10, "cache_read_input_tokens": nil}}}})
	if c.result(args{}, "") != nil {
		t.Fatal("explicit invalid cache counter counted as zero")
	}
	c.observe(args{"type": "system", "subtype": "compact_boundary"})
	if c.result(result, "fixture") != nil {
		t.Fatal("compaction kept stale context")
	}
}
func TestFileHookEnforcesPhysicalInboundOwnership(t *testing.T) {
	root := t.TempDir()
	room := filepath.Join(root, "room")
	work := filepath.Join(room, "work")
	home := filepath.Join(room, "home")
	inbound := filepath.Join(room, "inbound")
	other := filepath.Join(root, "other", "inbound")
	for _, p := range []string{work, home, inbound, other} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	w := &worker{workspace: core.Workspace{Root: room, Work: work, Home: home, Inbound: inbound}, events: make(chan core.WorkerEvent, 16), eventsDone: make(chan struct{})}
	check := func(tool, path string, wantDeny bool) {
		t.Helper()
		result, err := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": args{"file_path": path}})
		if err != nil {
			t.Fatal(err)
		}
		denied := object(result["hookSpecificOutput"])["permissionDecision"] == "deny"
		if denied != wantDeny {
			t.Fatalf("%s %s denied=%v want=%v", tool, path, denied, wantDeny)
		}
	}
	check("Read", filepath.Join(inbound, "image.png"), false)
	check("Write", filepath.Join(inbound, "image.png"), true)
	check("Read", filepath.Join(other, "secret.txt"), true)
	check("Write", filepath.Join(work, "answer.txt"), false)
	alias := filepath.Join(work, "external-link")
	if err := os.Symlink(other, alias); err == nil {
		check("Read", filepath.Join(alias, "new-child.txt"), true)
	}
	w.workspace.NativeConfig = "inherit"
	check("Read", filepath.Join(root, "external.txt"), false)
	check("Read", filepath.Join(other, "secret.txt"), true)
}
func TestYoloQuestionHookKeepsHumanInteraction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &worker{ctx: ctx, requests: map[string]chan args{}, events: make(chan core.WorkerEvent, 2), eventsDone: make(chan struct{})}
	done := make(chan args, 1)
	go func() {
		out, err := w.hook(ctx, "question", args{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_input": args{"questions": []any{args{"question": "Select?"}}}})
		if err != nil {
			done <- nil
			return
		}
		done <- out
	}()
	event := <-w.events
	if event.Kind != "question" {
		t.Fatal(event)
	}
	_ = w.Respond(ctx, event.ID, args{"approved": true, "answers": args{"Select?": "A"}})
	result := object((<-done)["hookSpecificOutput"])
	if result["permissionDecision"] != "allow" || object(object(result["updatedInput"])["answers"])["Select?"] != "A" {
		t.Fatal(result)
	}
}

func TestFileScopePinsCanonicalRootsAndRejectsInboundRedirection(t *testing.T) {
	root := t.TempDir()
	room := filepath.Join(root, "room")
	other := filepath.Join(root, "other", "inbound")
	for _, p := range []string{filepath.Join(room, "work"), filepath.Join(room, "home"), filepath.Join(room, "inbound"), other} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	w := &worker{workspace: core.Workspace{Root: room, Work: filepath.Join(room, "work"), Home: filepath.Join(room, "home"), Inbound: filepath.Join(room, "inbound"), NativeConfig: "inherit"}, events: make(chan core.WorkerEvent, 4), eventsDone: make(chan struct{})}
	if _, err := w.fileScope(); err != nil {
		t.Fatal(err)
	}
	// Replace the initially authorized path after scope capture. The new physical
	// target must not become authorized merely because the logical prefix matches.
	if err := os.Remove(w.workspace.Inbound); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, w.workspace.Inbound); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Windows symlink privilege unavailable")
		}
		t.Fatal(err)
	}
	result, err := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_input": args{"file_path": filepath.Join(w.workspace.Inbound, "other-chat.txt")}})
	if err != nil || object(result["hookSpecificOutput"])["permissionDecision"] != "deny" {
		t.Fatal("redirected inbound was authorized", result, err)
	}
	fresh := &worker{workspace: w.workspace}
	if _, err := fresh.fileScope(); err == nil {
		t.Fatal("preexisting inbound redirection was admitted")
	}
}
