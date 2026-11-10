package native

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// The test binary doubles as an independent CLI process, so these contracts
// exercise actual pipes, partial frames, cancellation and process ownership.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--output-format" {
		fakeClaude()
		os.Exit(0)
	}
	if len(os.Args) > 2 && os.Args[1] == "feishu-fixture-child" {
		for {
			_ = writeFixtureFile(os.Args[2], []byte(time.Now().Format(time.RFC3339Nano)))
			time.Sleep(10 * time.Millisecond)
		}
	}
	os.Exit(m.Run())
}

// The parent polls fixture files while this process writes them. Write to a
// sibling and rename so a poll never observes an empty or partial file.
func writeFixtureFile(path string, data []byte) error {
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func fakeClaude() {
	if dump := os.Getenv("FEISHU_NATIVE_TEST_ENV_DUMP"); dump != "" {
		var lines []string
		for _, key := range strings.Split(os.Getenv("FEISHU_NATIVE_TEST_ENV_KEYS"), ",") {
			if key != "" {
				lines = append(lines, key+"="+os.Getenv(key))
			}
		}
		if writeFixtureFile(dump, []byte(strings.Join(lines, "\n"))) != nil {
			os.Exit(34)
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	send := func(v args) { _ = encoder.Encode(v) }
	emitResult := func(uuid string, ids []string, value string) {
		send(args{"type": "result", "subtype": "success", "uuid": "result-" + uuid, "user_message_uuid": uuid, "user_message_uuids": ids, "result": value, "usage": args{"input_tokens": 9000}, "modelUsage": args{"fixture-model": args{"contextWindow": 321000}}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes+1)
	var held, current string
	pending := map[string]bool{}
	responses := args{}
	ready := false
	for scanner.Scan() {
		var frame args
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			os.Exit(22)
		}
		switch frame["type"] {
		case "control_request":
			request := object(frame["request"])
			switch request["subtype"] {
			case "initialize":
				hooks := object(request["hooks"])
				if len(array(hooks["PreToolUse"])) == 0 || len(stringsOf(request["sdkMcpServers"])) != 1 || !strings.Contains(text(request["appendSystemPrompt"]), "Feishu") {
					os.Exit(24)
				}
				send(args{"type": "control_response", "response": args{"subtype": "success", "request_id": frame["request_id"], "response": args{"hooks_applied": true}}})
			case "rename_session":
				title := text(request["title"])
				if title == "" || request["source"] != "host" {
					os.Exit(32)
				}
				if title == "fixture-reject-title" {
					send(args{"type": "control_response", "response": args{"subtype": "error", "request_id": frame["request_id"], "error": "fixture rejection"}})
					continue
				}
				if writeFixtureFile(filepath.Join(os.Getenv("HOME"), "session-title"), []byte(title)) != nil {
					os.Exit(33)
				}
				if title == "fixture-missing-ack" {
					continue
				}
				send(args{"type": "control_response", "response": args{"subtype": "success", "request_id": frame["request_id"]}})
			default:
				os.Exit(23)
			}
		case "control_response":
			response := object(frame["response"])
			id := text(response["request_id"])
			responses[id] = response
			delete(pending, id)
			if current != "" && len(pending) == 0 {
				data, _ := json.Marshal(responses)
				emitResult(current, []string{current}, string(data))
				current = ""
			}
		case "user":
			uuid := text(frame["uuid"])
			content := text(object(frame["message"])["content"])
			if !ready {
				id := "native-fixture"
				for i, arg := range os.Args {
					if arg == "--resume" && i+1 < len(os.Args) {
						id = os.Args[i+1]
						if id == "mismatch" {
							id = "different"
						}
					}
				}
				send(args{"type": "system", "subtype": "init", "session_id": id})
				ready = true
			}
			if object(frame["origin"])["kind"] != "human" || frame["parent_tool_use_id"] != nil {
				os.Exit(25)
			}
			send(args{"type": "user", "uuid": uuid, "origin": args{"kind": "human"}, "user_message_uuid": uuid})
			if strings.HasPrefix(content, "/compact") {
				if frame["priority"] != "later" || frame["client_composed"] != nil {
					os.Exit(26)
				}
				send(args{"type": "system", "subtype": "compact_boundary", "compact_metadata": args{"trigger": "manual"}})
				send(args{"type": "result", "subtype": "success", "local_command": "/compact", "uuid": "compact-result", "result": "", "user_message_uuid": uuid})
				continue
			}
			if frame["priority"] != "next" || frame["client_composed"] != true {
				os.Exit(27)
			}
			var envelope args
			_ = json.Unmarshal([]byte(content), &envelope)
			command := text(object(envelope["trigger"])["text"])
			send(args{"type": "system", "subtype": "session_state_changed", "state": "running"})
			switch command {
			case "hold":
				held = uuid
				continue
			case "finish":
				emitResult(held, []string{held, uuid}, "combined")
				held = ""
				continue
			case "orphan":
				path := filepath.Join(os.Getenv("HOME"), "heartbeat")
				child := exec.Command(os.Args[0], "feishu-fixture-child", path)
				child.Stdout = os.Stdout
				child.Stderr = os.Stderr
				if child.Start() != nil {
					os.Exit(28)
				}
				for i := 0; i < 100; i++ {
					if _, err := os.Stat(path); err == nil {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				os.Exit(31)
			case "cancel":
				id := "cancel-permission"
				current = uuid
				pending[id] = true
				send(args{"type": "control_request", "request_id": id, "request": args{"subtype": "can_use_tool", "tool_name": "Bash", "input": args{"command": "sleep 60"}}})
				send(args{"type": "control_cancel_request", "request_id": id})
				continue
			case "exercise":
				current = uuid
				requests := map[string]args{
					"permission": {"subtype": "can_use_tool", "tool_name": "Bash", "input": args{"command": "echo test"}, "tool_use_id": "tool-native"},
					"question":   {"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "input": args{"questions": []any{args{"question": "Pick?"}}}, "tool_use_id": "question-native"},
					"scope":      {"subtype": "hook_callback", "callback_id": "tool", "input": args{"hook_event_name": "PreToolUse", "tool_name": "Write", "tool_input": args{"file_path": filepath.Join(os.TempDir(), "not-this-conversation", "blocked.txt")}}},
					"precompact": {"subtype": "hook_callback", "callback_id": "compact", "input": args{"hook_event_name": "PreCompact", "trigger": "auto"}},
					"stop":       {"subtype": "hook_callback", "callback_id": "stop", "input": args{"hook_event_name": "Stop", "background_tasks": []any{args{"id": "bg"}}, "session_crons": []any{args{"id": "cron"}}}},
				}
				for name, msg := range map[string]args{"initialize": {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": args{"protocolVersion": "2025-11-25"}}, "tools": {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, "history": {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": args{"name": "read_chat_history", "arguments": args{"limit": 3}}}, "attachment": {"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": args{"name": "fetch_attachment", "arguments": args{"messageId": "m", "key": "k"}}}, "file": {"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": args{"name": "send_file", "arguments": args{"path": "answer.txt"}}}, "action": {"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": args{"name": "project_action", "arguments": args{"action": "read_status", "parameters": args{}}}}} {
					requests[name] = args{"subtype": "mcp_message", "server_name": "gateway", "message": msg}
				}
				for id, request := range requests {
					pending[id] = true
					send(args{"type": "control_request", "request_id": id, "request": request})
				}
				continue
			}
			send(args{"type": "stream_event", "event": args{"type": "message_start", "message": args{"id": "message-" + uuid, "model": "fixture-model", "usage": args{"input_tokens": 40, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 10}}}})
			send(args{"type": "stream_event", "event": args{"type": "content_block_start", "index": 0, "content_block": args{"type": "thinking", "thinking": ""}}})
			send(args{"type": "stream_event", "event": args{"type": "content_block_delta", "index": 0, "delta": args{"type": "thinking_delta", "thinking": "Visible reasoning"}}})
			send(args{"type": "stream_event", "event": args{"type": "content_block_start", "index": 1, "content_block": args{"type": "text", "text": ""}}})
			send(args{"type": "stream_event", "event": args{"type": "content_block_delta", "index": 1, "delta": args{"type": "text_delta", "text": "Hello"}}})
			send(args{"type": "assistant", "message": args{"id": "message-" + uuid, "model": "fixture-model", "content": []any{args{"type": "text", "text": "Hello"}}, "usage": args{"input_tokens": 0}}})
			emitResult(uuid, []string{uuid}, "Hello")
			send(args{"type": "system", "subtype": "session_state_changed", "state": "idle"})
		}
	}
}

type fixture struct {
	t         *testing.T
	config    core.Config
	workspace core.Workspace
	session   core.Session
	events    chan core.WorkerEvent
	mu        sync.Mutex
	all       []core.WorkerEvent
	worker    core.Worker
	callback  func(core.WorkerEvent)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	work := filepath.Join(root, "work")
	inbound := filepath.Join(root, "inbound")
	for _, path := range []string{home, work, inbound} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FEISHU_NATIVE_TEST_TOKEN", "fixture-secret")
	f := &fixture{t: t, config: core.Config{Runtime: core.RuntimeConfig{ClaudeExecutable: exe}, Model: core.ModelConfig{BaseURL: "http://127.0.0.1:1", TokenEnv: "FEISHU_NATIVE_TEST_TOKEN", Model: "fixture-model", SmallModel: "fixture-model"}, Projects: []core.Project{{ID: "p", PermissionMode: "default", Actions: []core.ActionSpec{{ID: "read_status", ReadOnly: true}}}}, Limits: core.Limits{InboxPerRoom: 8}}, workspace: core.Workspace{Root: root, Home: home, Work: work, Inbound: inbound, NativeConfig: "isolated"}, session: core.Session{ID: "session"}, events: make(chan core.WorkerEvent, 4096)}
	t.Cleanup(func() {
		if f.worker != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			if err := f.worker.Stop(ctx); err != nil {
				t.Errorf("stop: %v", err)
			}
		}
	})
	return f
}
func (f *fixture) start() {
	f.t.Helper()
	f.startWithLogger(nil)
}

func (f *fixture) startWithLogger(log core.Logger) {
	f.t.Helper()
	w, err := NewFactory(f.config, log)(context.Background(), core.Room{ProjectID: "p"}, f.session, f.workspace, func(e core.WorkerEvent) {
		f.mu.Lock()
		f.all = append(f.all, e)
		f.mu.Unlock()
		f.events <- e
		if f.callback != nil {
			f.callback(e)
		}
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.worker = w
}
func (f *fixture) send(value string) core.Delivery {
	f.t.Helper()
	d := core.Delivery{UUID: core.NewID(), SessionID: f.session.ID, Message: core.Message{ID: core.NewID(), Text: value}, Background: []core.Message{}, Attachments: []core.Attachment{}}
	if err := f.worker.Send(context.Background(), d); err != nil {
		f.t.Fatal(err)
	}
	return d
}
func (f *fixture) wait(check func(core.WorkerEvent) bool) core.WorkerEvent {
	f.t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e := <-f.events:
			if check(e) {
				return e
			}
		case <-timer.C:
			f.mu.Lock()
			defer f.mu.Unlock()
			f.t.Fatalf("event timeout: %+v", f.all)
			return core.WorkerEvent{}
		}
	}
}
func (f *fixture) event(typ string) core.WorkerEvent {
	return f.wait(func(e core.WorkerEvent) bool { return e.Type == typ })
}

func TestPersistentNativeStreamKeepsAttributionAndContext(t *testing.T) {
	f := newFixture(t)
	f.start()
	first := f.send("hello")
	result := f.event("result")
	if !result.Success || result.UUID != first.UUID || result.Context["inputTokens"] != int64(80) || result.Context["windowTokens"] != int64(321000) {
		t.Fatalf("result %+v", result)
	}
	second := f.send("again")
	if result = f.event("result"); result.UUID != second.UUID {
		t.Fatal(result)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ready, reasoning := 0, false
	for _, e := range f.all {
		if e.Type == "ready" {
			ready++
		}
		if e.Type == "thinking" && e.Text == "Visible reasoning" {
			reasoning = e.TurnID == first.UUID || e.TurnID == second.UUID
		}
		if e.Type == "text" && e.BlockID != "message-"+first.UUID+":1" && e.BlockID != "message-"+second.UUID+":1" {
			t.Fatalf("wrong streamed/completed text block: %+v", e)
		}
	}
	if ready != 1 || !reasoning {
		t.Fatalf("ready=%d reasoning=%v", ready, reasoning)
	}
}
func TestRenameSessionSetsNativeDisplayTitle(t *testing.T) {
	f := newFixture(t)
	f.start()
	if err := f.worker.Rename(context.Background(), " Release\n  Notes "); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.workspace.Home, "session-title")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if string(data) != "Release Notes" {
				t.Fatalf("native title %q", data)
			}
			if err = f.worker.Rename(context.Background(), " \n "); err == nil {
				t.Fatal("empty title accepted")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("native title was not applied")
}
func TestMCPAndHumanControlRequestsBridgeWithoutBlockingReadLoop(t *testing.T) {
	f := newFixture(t)
	f.callback = func(e core.WorkerEvent) {
		if e.Type == "request" {
			go func() {
				_ = f.worker.Respond(context.Background(), e.ID, args{"approved": true, "answers": args{"Pick?": "A"}, "ok": true, "result": "fixture"})
			}()
		}
	}
	f.start()
	f.send("exercise")
	result := f.event("result")
	var replies args
	if json.Unmarshal([]byte(result.Text), &replies) != nil {
		t.Fatal(result)
	}
	if len(replies) != 11 {
		t.Fatalf("got %d control replies: %s", len(replies), result.Text)
	}
	permission := object(object(replies["permission"])["response"])
	if permission["behavior"] != "allow" || permission["toolUseID"] != "tool-native" {
		t.Fatal(permission)
	}
	question := object(object(replies["question"])["response"])
	if object(object(question["updatedInput"])["answers"])["Pick?"] != "A" {
		t.Fatal(question)
	}
	scope := object(object(object(replies["scope"])["response"])["hookSpecificOutput"])
	if scope["permissionDecision"] != "deny" {
		t.Fatal(scope)
	}
	for _, name := range []string{"history", "attachment", "file", "action"} {
		rpc := object(object(replies[name])["response"])["mcp_response"]
		if len(array(object(object(rpc)["result"])["content"])) != 1 {
			t.Fatalf("%s: %v", name, rpc)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pinned := false
	for _, e := range f.all {
		if e.Type == "activity" && e.Pinned != nil && *e.Pinned && e.Background != nil && *e.Background == 1 {
			pinned = true
		}
	}
	if !pinned {
		t.Fatal("Stop hook lost live background/pinned state")
	}
}
func TestBusyInputLocalSupplementFenceAndNativeCompact(t *testing.T) {
	f := newFixture(t)
	f.start()
	first := f.send("hold")
	f.event("ready")
	second := core.Delivery{UUID: core.NewID(), SupplementTo: first.UUID, Message: core.Message{Text: "finish"}}
	if err := f.worker.Send(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	result := f.event("result")
	if len(result.UUIDs) != 2 {
		t.Fatal(result)
	}
	stale := second
	stale.UUID = core.NewID()
	if err := f.worker.Send(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	rejected := f.event("input_rejected")
	if rejected.UUID != stale.UUID {
		t.Fatal(rejected)
	}
	compact := core.Delivery{UUID: core.NewID(), Command: &core.Command{Name: "compact", Instructions: "keep unresolved work"}}
	if err := f.worker.Send(context.Background(), compact); err != nil {
		t.Fatal(err)
	}
	result = f.event("result")
	if result.Compact != "compacted" || result.UUID != compact.UUID || result.Context != nil {
		t.Fatal(result)
	}
}
func TestResumeMismatchIsFatalAndNeverAnnouncesReplacement(t *testing.T) {
	f := newFixture(t)
	f.session.CCID = "mismatch"
	f.start()
	f.send("hello")
	failure := f.event("error")
	if !strings.Contains(failure.Message, "silent reset") {
		t.Fatal(failure)
	}
	f.event("exit")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.all {
		if e.Type == "ready" {
			t.Fatal("replacement was announced", e)
		}
	}
}
func TestControlCancellationSettlesPendingHumanRequest(t *testing.T) {
	f := newFixture(t)
	f.start()
	f.send("cancel")
	f.event("result")
	w := f.worker.(*worker)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.requests)
		w.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled native control retained pending human request")
}
func TestUnexpectedCLIExitCleansDescendantBeforeExitEvent(t *testing.T) {
	f := newFixture(t)
	f.start()
	f.send("orphan")
	exit := f.event("exit")
	if exit.CleanupConfirmed != nil && !*exit.CleanupConfirmed {
		t.Fatal(exit)
	}
	path := filepath.Join(f.workspace.Home, "heartbeat")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("descendant still alive after worker exit")
	}
}
func TestPrivateSettingsAndEnvironmentDoNotExposeBridgeCredentials(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FEISHU_APP_SECRET", "app-secret")
	t.Setenv("CLAUDE_CODE_SESSION_FOO", "outer-session")
	t.Setenv("FEISHU_ACTION_TOKEN", "outer-action-token")
	t.Setenv("NODE_OPTIONS", "parent-node-control")
	f.workspace.NativeConfig = "inherit"
	f.config.AppSecretEnv = "FEISHU_APP_SECRET"
	env := Environment(f.workspace, f.config.Projects[0], f.config.AppSecretEnv)
	for _, key := range []string{"FEISHU_APP_SECRET", "CLAUDE_CODE_SESSION_FOO", "FEISHU_ACTION_TOKEN", "NODE_OPTIONS"} {
		if _, ok := env[key]; ok {
			t.Fatal("leaked environment", key)
		}
	}
	f.start()
	w := f.worker.(*worker)
	if w.settings == "" {
		t.Fatal("no private overlay")
	}
	data, err := os.ReadFile(w.settings)
	if err != nil || !strings.Contains(string(data), "fixture-secret") {
		t.Fatal(err)
	}
	for _, arg := range w.cmd.Args {
		if strings.Contains(arg, "fixture-secret") {
			t.Fatal("model secret in process arguments")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(w.settings); !os.IsNotExist(err) {
		t.Fatal("settings secret retained after termination")
	}
}
func TestProjectEnvironmentReachesNativeChild(t *testing.T) {
	f := newFixture(t)
	dump := filepath.Join(t.TempDir(), "native.env")
	f.config.Projects[0].Env = map[string]string{
		"PROFILE":                     "team",
		"FEISHU_NATIVE_TEST_ENV_DUMP": dump,
		"FEISHU_NATIVE_TEST_ENV_KEYS": "PROFILE",
	}
	f.start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(dump)
		if err == nil {
			if string(data) != "PROFILE=team" {
				t.Fatalf("child environment = %q", data)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("native child did not record its environment: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkerSecretsIncludeProjectPassthrough(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TEAM_API_TOKEN", "long-secret-value")
	f.config.Projects[0].EnvPassthrough = []string{"TEAM_API_TOKEN"}
	f.start()
	w := f.worker.(*worker)
	for _, secret := range w.secrets {
		if secret == "long-secret-value" {
			return
		}
	}
	t.Fatal("worker log redaction did not include the project passthrough value")
}

func TestQueueLimitAndDuplicateInputRejected(t *testing.T) {
	w := &worker{ctx: context.Background(), known: map[string]bool{}, pending: make(chan args, 1), events: make(chan core.WorkerEvent, 4), eventsDone: make(chan struct{})}
	d := core.Delivery{UUID: "one", Message: core.Message{Text: "hello"}}
	if err := w.Send(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(context.Background(), d); err == nil {
		t.Fatal("duplicate accepted")
	}
	d.UUID = "two"
	if err := w.Send(context.Background(), d); err == nil {
		t.Fatal("queue overflow accepted")
	}
}
func ExampleNewFactory() {
	fmt.Println("One admitted conversation, one persistent native Claude process.")
	// Output: One admitted conversation, one persistent native Claude process.
}

func TestWorkerExitReportsStopReasonAndTurnState(t *testing.T) {
	var mu sync.Mutex
	var exits []core.Args
	f := newFixture(t)
	f.startWithLogger(func(event string, value core.Args) {
		if event != "worker_exit" {
			return
		}
		mu.Lock()
		exits = append(exits, value)
		mu.Unlock()
	})
	f.worker.(interface{ SetStopReason(string) }).SetStopReason("idle_release")
	f.worker.(interface{ SetTurnActive(bool) }).SetTurnActive(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.worker.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(exits) != 1 {
		t.Fatalf("worker_exit logs = %#v", exits)
	}
	if exits[0]["reason"] != "idle_release" || exits[0]["inTurn"] != true {
		t.Fatalf("worker_exit diagnostics = %#v", exits[0])
	}
}

func TestObserveReportsToolResultActivity(t *testing.T) {
	w := &worker{output: newOutput(func(string) bool { return false }), known: map[string]bool{}, session: core.Session{}}
	events, err := w.observe(args{"type": "user", "message": args{"content": []any{args{"type": "tool_result", "tool_use_id": "tool-1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "activity" && event.Phase == "tool_end" && event.ToolUseID == "tool-1" {
			return
		}
	}
	t.Fatalf("tool result event missing: %+v", events)
}
