package native

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// Opt in with CLAUDE_NATIVE_PATH=/absolute/path/to/claude. Every model
// request is answered by the local deterministic server; no provider is used.
func TestRealClaudeNativeContract(t *testing.T) {
	executable := nativeTestExecutable(t)
	f := newFixture(t)
	f.config.Runtime.ClaudeExecutable = executable
	f.config.Model.ExtraHeaders = map[string]string{"x-fixture-project": "native-go"}
	var mu sync.Mutex
	var requests []args
	phase := "normal"
	call := 0
	target := filepath.Join(f.workspace.Work, "approved-file.txt")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("x-fixture-project") != "native-go" {
			t.Errorf("controlled model auth/headers missing")
			w.WriteHeader(401)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		var body args
		if json.Unmarshal(data, &body) != nil {
			w.WriteHeader(400)
			return
		}
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":6000}`)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		call++
		n, mode := call, phase
		mu.Unlock()
		var content []args
		if mode == "compact" {
			content = []args{{"type": "text", "text": "NATIVE_MEMORY_MARKER: history confirmed, approved-file.txt written, busy correction received; continue the same conversation."}}
		} else {
			switch n {
			case 1:
				content = []args{{"type": "tool_use", "id": "tool_history", "name": "mcp__gateway__read_chat_history", "input": args{"limit": 3}}}
			case 2:
				content = []args{{"type": "tool_use", "id": "tool_write", "name": "Write", "input": args{"file_path": target, "content": "native-go-approved"}}}
			case 3:
				content = []args{{"type": "tool_use", "id": "tool_question", "name": "AskUserQuestion", "input": args{"questions": []args{{"question": "Which option?", "header": "Option", "options": []args{{"label": "First", "description": "Use first option"}, {"label": "Second", "description": "Use second option"}}, "multiSelect": false}}}}}
			default:
				content = []args{{"type": "thinking", "thinking": "Visible native reasoning", "signature": "private-signature-must-not-be-shown"}, {"type": "text", "text": "NATIVE_MEMORY_MARKER: completed."}}
			}
		}
		writeModelResponse(w, body, fmt.Sprintf("msg_fixture_%d", n), content)
	}))
	defer server.Close()
	f.config.Model.BaseURL = server.URL
	var initialUUID string
	var callbackMu sync.Mutex
	permissionCount, questionCount, historyCount := 0, 0, 0
	f.callback = func(e core.WorkerEvent) {
		if e.Type != "request" {
			return
		}
		go func() {
			answer := args{"approved": false, "error": "unexpected fixture request"}
			switch e.Kind {
			case "history":
				callbackMu.Lock()
				historyCount++
				callbackMu.Unlock()
				answer = args{"messages": []args{{"id": "history-fixture", "senderId": "admin", "text": "HISTORY_LOCAL_RESULT"}}, "hasMore": false}
			case "question":
				callbackMu.Lock()
				questionCount++
				callbackMu.Unlock()
				answer = args{"approved": true, "answers": args{"Which option?": "First"}}
			case "permission":
				callbackMu.Lock()
				permissionCount++
				uuid := initialUUID
				callbackMu.Unlock()
				if e.Name != "Write" {
					t.Errorf("unexpected permission tool %q", e.Name)
				}
				supplement := core.Delivery{UUID: core.NewID(), SessionID: f.session.ID, SupplementTo: uuid, Message: core.Message{ID: core.NewID(), Text: "BUSY_LOCAL_CORRECTION"}}
				if err := f.worker.Send(context.Background(), supplement); err != nil {
					t.Error(err)
				}
				answer = args{"approved": true}
			}
			if err := f.worker.Respond(context.Background(), e.ID, answer); err != nil {
				t.Error(err)
			}
		}()
	}
	f.start()
	d := core.Delivery{UUID: core.NewID(), SessionID: f.session.ID, Message: core.Message{ID: core.NewID(), Text: "NATIVE_MEMORY_MARKER: start the local contract."}}
	callbackMu.Lock()
	initialUUID = d.UUID
	callbackMu.Unlock()
	if err := f.worker.Send(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	result := waitReal(f, "result", 45*time.Second)
	if !result.Success || len(result.UUIDs) < 2 {
		t.Fatalf("native result lost busy input: %+v", result)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "native-go-approved" {
		t.Fatalf("native approved write: %q %v", data, err)
	}
	callbackMu.Lock()
	counts := []int{historyCount, permissionCount, questionCount}
	callbackMu.Unlock()
	if counts[0] != 1 || counts[1] != 1 || counts[2] != 1 {
		t.Fatalf("native control counts history/permission/question=%v", counts)
	}
	mu.Lock()
	requestsJSON, _ := json.Marshal(requests)
	mu.Unlock()
	for _, marker := range []string{"HISTORY_LOCAL_RESULT", "BUSY_LOCAL_CORRECTION", "Which option?"} {
		if !strings.Contains(string(requestsJSON), marker) {
			t.Errorf("native provider context missing %s", marker)
		}
	}
	f.mu.Lock()
	sessionID := ""
	reasoning := false
	for _, e := range f.all {
		if e.Type == "ready" {
			sessionID = e.CCID
		}
		if e.Type == "thinking" && strings.Contains(e.Text, "Visible native reasoning") {
			reasoning = true
		}
		if strings.Contains(e.Text, "private-signature") {
			t.Error("reasoning signature exposed")
		}
	}
	f.mu.Unlock()
	if sessionID == "" || !reasoning {
		t.Fatalf("native session=%q reasoning=%v", sessionID, reasoning)
	}
	// Warm continuation must keep the same process and UUID provenance.
	warm := f.send("Continue warm.")
	result = waitReal(f, "result", 30*time.Second)
	if !result.Success || result.UUID != warm.UUID {
		t.Fatal(result)
	}
	// Compaction uses the native command at a turn boundary, retaining session ID.
	mu.Lock()
	phase = "compact"
	mu.Unlock()
	compact := core.Delivery{UUID: core.NewID(), SessionID: f.session.ID, Command: &core.Command{Name: "compact", Instructions: "keep NATIVE_MEMORY_MARKER"}}
	if err := f.worker.Send(context.Background(), compact); err != nil {
		t.Fatal(err)
	}
	result = waitReal(f, "result", 35*time.Second)
	if result.Compact != "compacted" {
		t.Fatalf("native manual compaction: %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	if err := f.worker.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitReal(f, "exit", 5*time.Second)
	mu.Lock()
	phase = "normal"
	before := len(requests)
	mu.Unlock()
	f.session.CCID = sessionID
	f.start()
	resumed := f.send("Continue after restart.")
	result = waitReal(f, "result", 35*time.Second)
	if !result.Success || result.UUID != resumed.UUID {
		t.Fatal(result)
	}
	mu.Lock()
	after := append([]args(nil), requests[before:]...)
	mu.Unlock()
	resumeJSON, _ := json.Marshal(after)
	if !strings.Contains(string(resumeJSON), "NATIVE_MEMORY_MARKER") {
		t.Fatal("resume lost compacted native conversation")
	}
	t.Logf("native %s: MCP, permissions, questions, busy input, warm continuation, compact, and persisted resume passed with local model", protocolVersion)
}
func waitReal(f *fixture, typ string, timeout time.Duration) core.WorkerEvent {
	f.t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case e := <-f.events:
			if e.Type == typ {
				return e
			}
			if e.Type == "error" || e.Type == "exit" && typ != "exit" {
				f.t.Fatalf("native failed awaiting %s: %+v", typ, e)
			}
		case <-timer.C:
			f.mu.Lock()
			defer f.mu.Unlock()
			f.t.Fatalf("native timed out awaiting %s: %+v", typ, f.all)
		}
	}
}
func writeModelResponse(w http.ResponseWriter, request args, id string, content []args) {
	stop := "end_turn"
	if content[len(content)-1]["type"] == "tool_use" {
		stop = "tool_use"
	}
	message := args{"id": id, "type": "message", "role": "assistant", "model": "fixture-model", "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": args{"input_tokens": 6000, "output_tokens": 200}}
	if !truth(request["stream"]) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, value args) {
		value["type"] = name
		data, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	start := clone(message)
	start["content"] = []args{}
	start["stop_reason"] = nil
	start["usage"] = args{"input_tokens": 6000, "output_tokens": 0}
	event("message_start", args{"message": start})
	for index, block := range content {
		kind := text(block["type"])
		begin := clone(block)
		var delta args
		switch kind {
		case "thinking":
			begin["thinking"] = ""
			begin["signature"] = ""
			delta = args{"type": "thinking_delta", "thinking": block["thinking"]}
		case "text":
			begin["text"] = ""
			delta = args{"type": "text_delta", "text": block["text"]}
		default:
			begin["input"] = args{}
			encoded, _ := json.Marshal(block["input"])
			delta = args{"type": "input_json_delta", "partial_json": string(encoded)}
		}
		event("content_block_start", args{"index": index, "content_block": begin})
		event("content_block_delta", args{"index": index, "delta": delta})
		if kind == "thinking" {
			event("content_block_delta", args{"index": index, "delta": args{"type": "signature_delta", "signature": block["signature"]}})
		}
		event("content_block_stop", args{"index": index})
	}
	event("message_delta", args{"delta": args{"stop_reason": stop, "stop_sequence": nil}, "usage": args{"output_tokens": 200}})
	event("message_stop", args{})
}

func nativeTestExecutable(t *testing.T) string {
	t.Helper()
	executable := os.Getenv("CLAUDE_NATIVE_PATH")
	if executable == "" {
		executable = os.Getenv("FEISHU_NATIVE_EXECUTABLE")
	}
	if executable == "" {
		t.Skip("set CLAUDE_NATIVE_PATH for local native CLI contract")
	}
	return executable
}

func TestRealClaudeRejectsMissingResumeWithoutModelCall(t *testing.T) {
	executable := nativeTestExecutable(t)
	f := newFixture(t)
	f.config.Runtime.ClaudeExecutable = executable
	f.session.CCID = core.NewID()
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); calls++; mu.Unlock(); w.WriteHeader(500) }))
	defer server.Close()
	f.config.Model.BaseURL = server.URL
	f.start()
	f.send("Do not silently replace this missing transcript")
	waitReal(f, "exit", 20*time.Second)
	mu.Lock()
	count := calls
	mu.Unlock()
	if count != 0 {
		t.Fatalf("missing resume reached model %d times", count)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.all {
		if e.Type == "ready" {
			t.Fatal("missing resume created another conversation", e)
		}
	}
}

func TestRealClaudeProjectPromptReachesSystem(t *testing.T) {
	executable := nativeTestExecutable(t)
	f := newFixture(t)
	f.config.Runtime.ClaudeExecutable = executable
	f.config.Projects[0].AppendSystemPrompt = "PROJECT_PROMPT_MARKER_321"
	var mu sync.Mutex
	system := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var body args
		_ = json.Unmarshal(data, &body)
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens": 1}`)
			return
		}
		mu.Lock()
		candidate := ""
		switch v := body["system"].(type) {
		case string:
			candidate = v
		case []any:
			var b strings.Builder
			for _, item := range v {
				b.WriteString(text(object(item)["text"]))
			}
			candidate = b.String()
		}
		if strings.Contains(candidate, "PROJECT_PROMPT_MARKER_321") {
			system = candidate
		}
		mu.Unlock()
		writeModelResponse(w, body, "msg_project_prompt", []args{{"type": "text", "text": "project prompt fixture"}})
	}))
	defer server.Close()
	f.config.Model.BaseURL = server.URL
	f.start()
	d := f.send("Show the project prompt.")
	result := waitReal(f, "result", 30*time.Second)
	if !result.Success || result.UUID != d.UUID {
		t.Fatalf("project prompt result = %+v", result)
	}
	mu.Lock()
	captured := system
	mu.Unlock()
	if captured == "" {
		t.Fatal("project prompt did not appear in the native system field")
	}
	bridge, project := strings.Index(captured, "Feishu"), strings.Index(captured, "Project instructions:")
	if bridge < 0 || project < 0 || bridge > project {
		t.Fatalf("project prompt order: bridge=%d project=%d", bridge, project)
	}
}

func TestRealClaudeProjectProfileResumeKeepsConversation(t *testing.T) {
	executable := nativeTestExecutable(t)
	f := newFixture(t)
	f.config.Runtime.ClaudeExecutable = executable
	f.config.ModelProfiles = map[string]core.ModelProfile{
		"first":  {Model: "profile-model-first"},
		"second": {Model: "profile-model-second"},
	}
	f.config.Projects[0].ModelProfile = "first"
	var mu sync.Mutex
	var requests []args
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var body args
		_ = json.Unmarshal(data, &body)
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens": 1}`)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		n := len(requests)
		mu.Unlock()
		writeModelResponse(w, body, fmt.Sprintf("msg_project_profile_%d", n), []args{{"type": "text", "text": "profile resume fixture"}})
	}))
	defer server.Close()
	f.config.Model.BaseURL = server.URL
	f.start()
	d := f.send("FIRST_TURN_ONLY_MARKER from the initial project profile turn")
	result := waitReal(f, "result", 30*time.Second)
	if !result.Success || result.UUID != d.UUID {
		t.Fatalf("initial project profile result = %+v", result)
	}
	f.mu.Lock()
	sessionID := ""
	for _, e := range f.all {
		if e.Type == "ready" {
			sessionID = e.CCID
		}
	}
	f.mu.Unlock()
	if sessionID == "" {
		t.Fatal("native did not report a session ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	err := f.worker.Stop(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	waitReal(f, "exit", 5*time.Second)
	f.session.CCID = sessionID
	f.config.Projects[0].ModelProfile = "second"
	mu.Lock()
	before := len(requests)
	mu.Unlock()
	f.start()
	d = f.send("Continue the same native conversation.")
	result = waitReal(f, "result", 35*time.Second)
	if !result.Success || result.UUID != d.UUID {
		t.Fatalf("resumed project profile result = %+v", result)
	}
	mu.Lock()
	after := append([]args(nil), requests[before:]...)
	mu.Unlock()
	if len(after) == 0 {
		t.Fatal("resumed native process made no model request")
	}
	if got := text(after[0]["model"]); got != "profile-model-second" {
		t.Fatalf("resume did not switch to the changed project default: %q", got)
	}
	encoded, _ := json.Marshal(after)
	if !strings.Contains(string(encoded), "FIRST_TURN_ONLY_MARKER") {
		t.Fatal("changed project default did not preserve native conversation history")
	}
}
