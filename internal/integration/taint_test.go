package integration_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
)

// TestRealNativeTaintedHookBlocksWriteUntilApproval exercises the actual CLI
// PreToolUse callback: while the tainted turn waits for approval, the Bash tool
// must not run; an approved response lets it proceed.
func TestRealNativeTaintedHookBlocksWriteUntilApproval(t *testing.T) {
	executable := os.Getenv("CLAUDE_NATIVE_PATH")
	if executable == "" {
		t.Skip("set CLAUDE_NATIVE_PATH for the real native taint contract")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	inbound := filepath.Join(root, "inbound")
	for _, path := range []string{work, home, inbound} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FEISHU_INTEGRATION_MODEL_TOKEN", "integration-fixture-token")
	marker := filepath.Join(work, "executed.txt")
	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") || r.Header.Get("Authorization") != "Bearer integration-fixture-token" {
			t.Errorf("model request escaped the local fixture: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var body core.Args
		if err != nil || json.Unmarshal(data, &body) != nil || body["model"] != "fixture-model" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":6000}`)
			return
		}
		n := modelCalls.Add(1)
		var content []core.Args
		if n == 1 {
			content = []core.Args{{"type": "tool_use", "id": "tainted-write", "name": "Bash", "input": core.Args{"command": "printf executed > executed.txt"}}}
		} else {
			content = []core.Args{{"type": "text", "text": "TAINT_DONE"}}
		}
		writeModel(w, body, fmt.Sprintf("taint-model-%d", n), content)
	}))
	t.Cleanup(server.Close)

	c := config.Defaults()
	c.TenantID, c.AppID, c.AppSecretEnv = "fixture-tenant", "fixture-app", "FEISHU_INTEGRATION_UNUSED_SECRET"
	c.DataDir = filepath.Join(root, "state")
	c.Runtime.ClaudeExecutable = executable
	c.Runtime.MaxWorkers, c.Runtime.IdleMS = 1, 60000
	c.Model.BaseURL, c.Model.AllowHTTP = server.URL, true
	c.Model.TokenEnv = "FEISHU_INTEGRATION_MODEL_TOKEN"
	c.Model.Model, c.Model.SmallModel = "fixture-model", "fixture-model"
	c.Projects = []core.Project{{ID: "fixture", CWD: work, NativeConfig: "isolated", PermissionMode: "bypassPermissions", Maintainers: []string{"owner"}}}
	events := make(chan core.WorkerEvent, 256)
	factory := native.NewFactory(c, func(string, core.Args) {})
	worker, err := factory(ctx, core.Room{Key: "room-key", ProjectID: "fixture"}, core.Session{ID: "session"}, core.Workspace{Root: root, Work: work, Home: home, Inbound: inbound, NativeConfig: "isolated"}, func(event core.WorkerEvent) { events <- event })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer stopCancel()
		if err := worker.Stop(stopCtx); err != nil {
			t.Errorf("worker cleanup: %v", err)
		}
	})
	provider, ok := worker.(core.TaintProvider)
	if !ok {
		t.Fatal("native worker does not expose taint state")
	}
	provider.SetTaintCheck(func() bool { return true })
	delivery := core.Delivery{UUID: core.NewID(), SessionID: "session", Message: core.Message{ID: "one", ChatID: "chat", ChatType: "group", SenderID: "member", Text: "run the fixture", Time: time.Now().UnixMilli()}}
	if err := worker.Send(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	var request core.WorkerEvent
	await(t, ctx, "tainted PreToolUse approval request", func() bool {
		for {
			select {
			case event := <-events:
				if event.Type == "request" && event.Tainted && event.Name == "Bash" {
					request = event
					return true
				}
			default:
				return false
			}
		}
	})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Bash ran before approval: %v", err)
	}
	if err := worker.Respond(ctx, request.ID, core.Args{"approved": true}); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, "approved Bash result", func() bool {
		data, err := os.ReadFile(marker)
		return err == nil && strings.TrimSpace(string(data)) == "executed"
	})
}
