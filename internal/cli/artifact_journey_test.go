package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
)

type artifactLogs struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *artifactLogs) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(data)
}
func (l *artifactLogs) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buffer.String() }

// This runs the exact production artifact through real TLS/WS/HTTP adapters,
// SQLite, process management and native Claude Code. Only the platform and
// model services are local fixtures; no alternate service factory is linked.
func TestBuiltArtifactNativeApprovalFileReconnectAndResumeJourney(t *testing.T) {
	identity := readArtifactIdentity(t, "FEISHU_ARTIFACT")
	runArtifactNativeJourney(t, identity, identity)
}

func TestBuiltArtifactUpgradePreservesServiceDataAndNativeSession(t *testing.T) {
	current := readArtifactIdentity(t, "FEISHU_ARTIFACT")
	previous := readArtifactIdentity(t, "FEISHU_PREVIOUS_ARTIFACT")
	if previous.Commit == current.Commit || previous.SHA256 == current.SHA256 || previous.Version == current.Version {
		t.Fatal("upgrade requires distinct real old/new artifacts, source commits and version identities")
	}
	runArtifactNativeJourney(t, previous, current)
}

func runArtifactNativeJourney(t *testing.T, initial, current artifactIdentity) {
	t.Helper()
	binary := initial.Path
	claude := os.Getenv("CLAUDE_NATIVE_PATH")
	if claude == "" {
		t.Fatal("real native CLI and exact artifact version are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	dir := t.TempDir()
	upgrade := initial.Commit != current.Commit
	if upgrade {
		binary = filepath.Join(dir, filepath.Base(current.Path))
		installArtifactBinary(t, initial, binary)
	}
	work := filepath.Join(dir, "project")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(work, "artifact-approved.txt")
	complete := strings.Repeat("完整产物回复 ", 4000) + "ARTIFACT_COMPLETE_END"
	var calls, history, resumed atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") || r.Header.Get("Authorization") != "Bearer artifact-native-model" {
			t.Errorf("native model request escaped fixture: %s %s", r.Method, r.URL.Path)
			http.Error(w, "fixture only", 400)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var body core.Args
		if err != nil || json.Unmarshal(data, &body) != nil || body["model"] != "fixture-model" {
			http.Error(w, "invalid native fixture request", 400)
			return
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":6000}`)
			return
		}
		n := calls.Add(1)
		var content []core.Args
		switch n {
		case 1:
			content = []core.Args{{"type": "tool_use", "id": "artifact_history", "name": "mcp__gateway__read_chat_history", "input": core.Args{"limit": 3}}}
		case 2:
			if strings.Contains(string(data), "ARTIFACT_HISTORY_MARKER") {
				history.Add(1)
			}
			content = []core.Args{{"type": "tool_use", "id": "artifact_write", "name": "Write", "input": core.Args{"file_path": target, "content": "artifact-approved-native"}}}
		case 3:
			content = []core.Args{{"type": "tool_use", "id": "artifact_question", "name": "AskUserQuestion", "input": core.Args{"questions": []core.Args{{"question": "Which artifact delivery?", "header": "Delivery", "options": []core.Args{{"label": "First", "description": "Use the first fixture choice"}, {"label": "Second", "description": "Use the second fixture choice"}}, "multiSelect": false}}}}}
		case 4:
			if !strings.Contains(string(data), "First") || !strings.Contains(string(data), "Which artifact delivery?") {
				t.Error("answer after reconnect did not reach native question result")
			}
			content = []core.Args{{"type": "text", "text": complete}}
		default:
			if strings.Contains(string(data), "ARTIFACT_COMPLETE_END") && strings.Contains(string(data), "artifact-approved-native") && strings.Contains(string(data), "ARTIFACT_HISTORY_MARKER") {
				resumed.Add(1)
			}
			content = []core.Args{{"type": "text", "text": "ARTIFACT_RESUMED_REPLY"}}
		}
		writeArtifactModel(w, body, fmt.Sprintf("artifact_model_%d", n), content)
	}))
	defer model.Close()
	platform := newArtifactPlatform(t)
	defer platform.releaseFile()
	proxy, certificate, _ := artifactFeishuProxy(t, http.HandlerFunc(platform.serveHTTP))
	defer proxy.Close()
	trust := artifactTrustEnvironment(t, certificate, dir)
	c := config.Defaults()
	c.TenantID, c.AppID, c.AppSecretEnv = "tenant_fixture", "cli_fixture", "ARTIFACT_NATIVE_APP_SECRET"
	c.DataDir = filepath.Join(dir, "state")
	c.Runtime.ClaudeExecutable = claude
	c.Runtime.MaxWorkers = 1
	c.Runtime.IdleMS = 60000
	c.Model.BaseURL, c.Model.AllowHTTP = model.URL, true
	c.Model.TokenEnv, c.Model.Model, c.Model.SmallModel = "ARTIFACT_NATIVE_MODEL_TOKEN", "fixture-model", "fixture-model"
	c.Projects = []core.Project{{ID: "fixture", CWD: work, NativeConfig: "isolated", PermissionMode: "default", Maintainers: []string{"ou_owner"}}}
	c.Groups = []core.Group{{ChatID: "oc_artifact", ProjectID: "fixture", Members: []string{"ou_member"}}}
	c.DMs = nil
	c.Limits.UpdateMS, c.Limits.ApprovalMS = 250, 60000
	configPath := filepath.Join(dir, "config.json")
	// Keep the upgrade input in the baseline schema. New optional configuration
	// defaults must not make a real previous binary unable to start this fixture.
	encoded, _ := json.Marshal(core.Args{
		"tenantId": c.TenantID, "appId": c.AppID, "appSecretEnv": c.AppSecretEnv, "dataDir": c.DataDir,
		"runtime":  core.Args{"claudeExecutable": claude, "maxWorkers": 1, "idleMs": 60000},
		"model":    core.Args{"baseUrl": model.URL, "allowHttp": true, "tokenEnv": c.Model.TokenEnv, "model": "fixture-model", "smallModel": "fixture-model"},
		"projects": []core.Args{{"id": "fixture", "cwd": work, "nativeConfig": "isolated", "permissionMode": "default", "maintainers": []string{"ou_owner"}, "actions": []any{}}},
		"groups":   []core.Args{{"chatId": "oc_artifact", "projectId": "fixture", "members": []string{"ou_member"}}}, "dms": []any{},
		"limits": core.Args{"updateMs": 250, "approvalMs": 60000},
	})
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HTTPS_PROXY="+proxy.URL, "HTTP_PROXY="+proxy.URL, "ALL_PROXY="+proxy.URL, "NO_PROXY=127.0.0.1,localhost", "https_proxy="+proxy.URL, "http_proxy="+proxy.URL, "all_proxy="+proxy.URL, "no_proxy=127.0.0.1,localhost")
	env = append(env, trust...)
	envPath := filepath.Join(dir, "service.env")
	envData := []byte("ARTIFACT_NATIVE_APP_SECRET=artifact-native-app\nARTIFACT_NATIVE_MODEL_TOKEN=artifact-native-model\n")
	if err := os.WriteFile(envPath, envData, 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "service.log")
	verifyVersion := func(expected string) {
		versionCmd := exec.Command(binary, "--version")
		versionCmd.Env = env
		var version bytes.Buffer
		versionCmd.Stdout = &version
		if err := process.Run(ctx, versionCmd); err != nil || strings.TrimSpace(version.String()) != "feishu-bot "+expected {
			t.Fatal("artifact journey version identity mismatch", err, version.String())
		}
	}
	verifyVersion(initial.Version)
	var logs artifactLogs
	var stopService func()
	start := func() *artifactWire {
		serviceCtx, serviceCancel := context.WithCancel(ctx)
		cmd := exec.Command(binary, "run", "--service", "--config", configPath, "--env-file", envPath, "--log-file", logPath)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = &logs, &logs
		done := make(chan error, 1)
		go func() { done <- process.Run(serviceCtx, cmd) }()
		var once sync.Once
		stopService = func() {
			once.Do(func() {
				_ = os.WriteFile(filepath.Join(c.DataDir, "gateway.stop"), []byte("stop\n"), 0600)
				select {
				case err := <-done:
					if err != nil {
						t.Error("artifact service stop", err, logs.String())
					}
				case <-time.After(15 * time.Second):
					serviceCancel()
					select {
					case <-done:
					case <-time.After(8 * time.Second):
						t.Error("artifact process cleanup unconfirmed")
					}
					t.Error("artifact did not stop through its production control file")
				}
				serviceCancel()
			})
		}
		select {
		case wire := <-platform.connections:
			return wire
		case err := <-done:
			once.Do(serviceCancel)
			t.Fatalf("artifact exited before its WS connection: %v\n%s", err, logs.String())
		case <-ctx.Done():
			t.Fatalf("artifact WS startup timed out: %v\n%s", ctx.Err(), logs.String())
		}
		return nil
	}
	defer func() {
		platform.releaseFile()
		if stopService != nil {
			stopService()
		}
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	wire := start()
	platform.message(t, ctx, wire, "om_initial", "Run the local artifact journey.", "ou_member")
	permission, value := platform.button(t, ctx, "批准一次")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("native Write ran before approval", err)
	}
	// Poll a read-only rejection until the HTTP-created card is registered in
	// the gateway. No authorized approval or model task is retried.
	for {
		answer := platform.action(t, ctx, wire, permission.ID, "ou_member", value)
		if answer == "仅维护者可以审批" {
			break
		}
		if answer != "" {
			t.Fatal("unexpected unauthorized permission result", answer)
		}
		if ctx.Err() != nil {
			t.Fatal("permission card registration timed out")
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("ordinary member approval executed Write", err)
	}
	if answer := platform.action(t, ctx, wire, permission.ID, "ou_owner", value); answer != "已批准一次" {
		t.Fatal("maintainer approval failed", answer)
	}
	_, question := platform.button(t, ctx, "提交回答", "提交选择")
	written, err := os.ReadFile(target)
	if err != nil || string(written) != "artifact-approved-native" {
		t.Fatal("approved native Write did not execute", err)
	}
	request, _ := question["request"].(string)
	if request == "" {
		t.Fatal("question lost its request binding")
	}
	// A replacement WS connection must preserve the live native approval and
	// input handlers. The answer is delivered through the actual wire parser.
	_ = wire.conn.Close()
	wire = platform.connected(t, ctx)
	platform.message(t, ctx, wire, "om_answer", "/answer "+request+" First", "ou_member")
	select {
	case text := <-platform.fileSeen:
		if text != complete {
			t.Fatalf("artifact export lost content: got %d bytes, want %d", len(text), len(complete))
		}
	case <-ctx.Done():
		t.Fatal("artifact did not upload the complete reply")
	}
	if platform.isDone("om_initial") {
		t.Fatal("artifact marked Done before full file acknowledgement")
	}
	platform.releaseFile()
	platform.wait(t, ctx, "final Done", func() bool { return platform.isDone("om_initial") })
	platform.mu.Lock()
	replyCards, files, earlyDone := 0, platform.fileMessages, platform.doneBeforeFileAck
	for _, card := range platform.cards {
		if card.ReplyTo == "om_initial" {
			replyCards++
		}
	}
	platform.mu.Unlock()
	if replyCards != 1 || files != 1 || earlyDone {
		t.Fatalf("artifact duplicated or prematurely confirmed reply/file delivery: cards=%d files=%d earlyDone=%t", replyCards, files, earlyDone)
	}
	platform.message(t, ctx, wire, "om_stop", "/stop", "ou_owner")
	stopService()
	original, cc := artifactSession(t, c.DataDir)
	beforeLog, err := os.ReadFile(logPath)
	if err != nil || len(beforeLog) == 0 {
		t.Fatal("production service log was not written", err)
	}
	if upgrade {
		installArtifactBinary(t, current, binary)
		verifyVersion(current.Version)
	}
	wire = start()
	platform.message(t, ctx, wire, "om_resumed", "Continue the existing artifact conversation.", "ou_member")
	platform.wait(t, ctx, "resumed Done", func() bool { return platform.isDone("om_resumed") })
	stopService()
	after, afterCC := artifactSession(t, c.DataDir)
	if original != after || cc == "" || cc != afterCC || history.Load() != 1 || resumed.Load() != 1 || calls.Load() != 5 {
		t.Fatalf("artifact changed session or replayed native tools: sameSession=%t sameNative=%t history=%d resumed=%d calls=%d", original == after, cc == afterCC, history.Load(), resumed.Load(), calls.Load())
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "gateway.lock")); !os.IsNotExist(err) {
		t.Fatal("artifact left its service lock after confirmed shutdown", err)
	}
	for path, expected := range map[string][]byte{configPath: encoded, envPath: envData, target: []byte("artifact-approved-native")} {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatal("service restart/upgrade changed configuration, environment or project data", filepath.Base(path), err)
		}
	}
	afterLog, err := os.ReadFile(logPath)
	if err != nil || !bytes.HasPrefix(afterLog, beforeLog) || len(afterLog) <= len(beforeLog) {
		t.Fatal("service restart/upgrade replaced or stopped appending its existing log", err)
	}
	t.Logf("Actual production binary + real native CLI + local TLS/WS/REST/model: MCP history, maintainer Write approval, question answer after reconnect, same reply card, complete file before Done, /stop, service restart and original native session/history all confirmed. upgrade=%t oldCommit=%s newCommit=%s; config/env/project bytes and service-log prefix preserved.", upgrade, initial.Commit, current.Commit)
}

func artifactSession(t *testing.T, dir string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "gateway.sqlite")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatal("artifact database is missing", err)
	}
	// Inspect without store.Open: the test harness must not migrate an old
	// binary's database before the new production binary gets to open it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	var id, cc string
	if err := db.QueryRow("SELECT id,cc FROM sessions WHERE state='open'").Scan(&id, &cc); err != nil {
		t.Fatal("artifact did not persist its native session", err)
	}
	return id, cc
}
func writeArtifactModel(w http.ResponseWriter, request core.Args, id string, content []core.Args) {
	stop := "end_turn"
	for _, part := range content {
		if part["type"] == "tool_use" {
			stop = "tool_use"
		}
	}
	message := core.Args{"id": id, "type": "message", "role": "assistant", "model": "fixture-model", "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": core.Args{"input_tokens": 6000, "output_tokens": 200, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}
	if streaming, _ := request["stream"].(bool); !streaming {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(kind string, value core.Args) {
		value["type"] = kind
		data, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	message["content"], message["stop_reason"] = []core.Args{}, nil
	message["usage"] = core.Args{"input_tokens": 6000, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	emit("message_start", core.Args{"message": message})
	for index, part := range content {
		initial := core.Args{"type": "text", "text": ""}
		delta := core.Args{"type": "text_delta", "text": part["text"]}
		if part["type"] == "tool_use" {
			input, _ := json.Marshal(part["input"])
			initial = core.Args{"type": "tool_use", "id": part["id"], "name": part["name"], "input": core.Args{}}
			delta = core.Args{"type": "input_json_delta", "partial_json": string(input)}
		}
		emit("content_block_start", core.Args{"index": index, "content_block": initial})
		emit("content_block_delta", core.Args{"index": index, "delta": delta})
		emit("content_block_stop", core.Args{"index": index})
	}
	emit("message_delta", core.Args{"delta": core.Args{"stop_reason": stop, "stop_sequence": nil}, "usage": core.Args{"output_tokens": 200}})
	emit("message_stop", core.Args{})
}
