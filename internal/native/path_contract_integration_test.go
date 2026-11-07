package native

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// These inputs go through the pinned CLI, including its path normalization and
// actual tool implementation. Hook-only tests cannot establish that ordering.
func TestRealClaudeFileToolsRespectConversationPaths(t *testing.T) {
	executable := nativeTestExecutable(t)
	f := newFixture(t)
	f.config.Runtime.ClaudeExecutable = executable
	room := filepath.Join(t.TempDir(), "rooms", "chat")
	f.workspace = core.Workspace{Root: room, Work: filepath.Join(room, "workspace"), Home: filepath.Join(room, "home"), Inbound: filepath.Join(room, "topic-inbound", "mine"), NativeConfig: "isolated"}
	other := filepath.Join(room, "topic-inbound", "other")
	for _, path := range []string{f.workspace.Work, f.workspace.Home, f.workspace.Inbound, other} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	allowed := filepath.Join(f.workspace.Work, "allowed.txt")
	owned := filepath.Join(f.workspace.Inbound, "own-upload.txt")
	forbidden := filepath.Join(other, "PRIVATE_OTHER_UPLOAD.txt")
	for path, value := range map[string]string{allowed: "PUBLIC_PATH_MARKER", owned: "OWN_UPLOAD_MARKER", forbidden: "OTHER_TOPIC_SECRET_MARKER"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	type testCase struct {
		id, tool string
		input    args
		want     string
		denied   bool
	}
	cases := []testCase{
		{"read_work", "Read", args{"file_path": allowed}, "PUBLIC_PATH_MARKER", false},
		{"read_own_upload", "Read", args{"file_path": owned}, "OWN_UPLOAD_MARKER", false},
		{"read_other", "Read", args{"file_path": forbidden}, "", true},
		{"read_expanded_home", "Read", args{"file_path": "~/../topic-inbound/other/PRIVATE_OTHER_UPLOAD.txt"}, "", true},
		{"read_trimmed_path", "Read", args{"file_path": " " + forbidden + " "}, "", true},
		{"grep_work", "Grep", args{"path": ".", "pattern": "PUBLIC_PATH_MARKER", "output_mode": "content"}, "PUBLIC_PATH_MARKER", false},
		{"grep_other", "Grep", args{"path": "../topic-inbound/other", "pattern": ".", "output_mode": "content"}, "", true},
		{"glob_work", "Glob", args{"pattern": "*.txt"}, "allowed.txt", false},
		{"glob_other", "Glob", args{"pattern": "../topic-inbound/other/*.txt"}, "", true},
	}
	alias := filepath.Join(f.workspace.Work, "other-upload-alias")
	if err := os.Symlink(other, alias); err == nil {
		cases = append(cases,
			testCase{"read_alias", "Read", args{"file_path": filepath.Join(alias, "PRIVATE_OTHER_UPLOAD.txt")}, "", true},
			testCase{"grep_alias", "Grep", args{"path": alias, "pattern": ".", "output_mode": "content"}, "", true},
			testCase{"glob_alias", "Glob", args{"pattern": filepath.Join(alias, "*.txt")}, "", true},
		)
	} else {
		t.Logf("symlink subcases unavailable on this runner: %v", err)
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			testCase{"read_msys", "Read", args{"file_path": msysForm(forbidden)}, "", true},
			testCase{"grep_msys", "Grep", args{"path": msysForm(other), "pattern": ".", "output_mode": "content"}, "", true},
			testCase{"glob_msys", "Glob", args{"pattern": msysForm(other) + "/*.txt"}, "", true},
		)
	}
	var mu sync.Mutex
	requests := []args{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body args
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":6000}`)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		first := len(requests) == 1
		mu.Unlock()
		content := []args{{"type": "text", "text": "Path contract checked."}}
		if first {
			content = nil
			for _, tc := range cases {
				content = append(content, args{"type": "tool_use", "id": tc.id, "name": tc.tool, "input": tc.input})
			}
		}
		writeModelResponse(w, body, core.NewID(), content)
	}))
	defer server.Close()
	f.config.Model.BaseURL = server.URL
	f.callback = func(event core.WorkerEvent) {
		if event.Type == "request" {
			go func() {
				_ = f.worker.Respond(context.Background(), event.ID, args{"approved": false, "error": "fixture has no extra permissions"})
			}()
		}
	}
	f.start()
	f.send("Exercise the file tools against these test-owned paths.")
	result := waitReal(f, "result", 60*time.Second)
	if !result.Success {
		t.Fatalf("native path fixture did not finish: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	results := map[string]args{}
	for _, request := range requests {
		for _, rawMessage := range array(request["messages"]) {
			for _, rawBlock := range array(object(rawMessage)["content"]) {
				block := object(rawBlock)
				if block["type"] == "tool_result" {
					results[text(block["tool_use_id"])] = block
				}
			}
		}
	}
	for _, tc := range cases {
		block, ok := results[tc.id]
		encoded, _ := json.Marshal(block["content"])
		if !ok {
			t.Errorf("native did not return %s", tc.id)
			continue
		}
		if tc.denied {
			if !truth(block["is_error"]) || strings.Contains(string(encoded), "OTHER_TOPIC_SECRET_MARKER") {
				t.Errorf("%s did not reject the protected path: %s", tc.id, encoded)
			}
		} else if truth(block["is_error"]) || !strings.Contains(string(encoded), tc.want) {
			t.Errorf("%s rejected a permitted file: %s", tc.id, encoded)
		}
	}
}
