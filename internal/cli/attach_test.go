package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

func TestParseAttach(t *testing.T) {
	o, err := Parse([]string{"attach", "--config", "cfg.json", "--user", "ou_dm", "--title", "hello", "--summary-file", "-", "--native", "native-id", "--holder-pid", "42", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Command != "attach" || o.AttachUser != "ou_dm" || o.AttachTitle != "hello" || o.AttachSummaryFile != "-" || o.AttachNative != "native-id" || o.AttachHolderPID != 42 || !o.AttachHolderSet || !o.JSON {
		t.Fatalf("options: %+v", o)
	}
	for _, args := range [][]string{
		{"attach", "--summary-file"},
		{"attach", "--holder-pid", "-1"},
		{"attach", "--json", "--json"},
		{"attach", "--unknown"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func attachConfig(t *testing.T, root string) core.Config {
	t.Helper()
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = physical
	return core.Config{
		TenantID: "tenant", AppID: "app", DataDir: t.TempDir(),
		Model:    core.ModelConfig{Model: "test-model"},
		Projects: []core.Project{{ID: "project", NativeConfig: "inherit"}},
		DMs:      []core.DM{{UserID: "ou_dm", ProjectID: "project", AdoptRoots: []string{root}}},
	}
}

func TestAttachTarget(t *testing.T) {
	root := t.TempDir()
	c := attachConfig(t, root)
	if user, err := attachTarget(c, Options{}); err != nil || user != "ou_dm" {
		t.Fatalf("single target: %q %v", user, err)
	}
	c.DMs = append(c.DMs, core.DM{UserID: "ou_other", ProjectID: "project", AdoptRoots: []string{root}})
	if _, err := attachTarget(c, Options{}); err == nil {
		t.Fatal("ambiguous target was accepted")
	}
	if user, err := attachTarget(c, Options{AttachUser: "ou_other"}); err != nil || user != "ou_other" {
		t.Fatalf("explicit target: %q %v", user, err)
	}
	c.DMs = nil
	if _, err := attachTarget(c, Options{}); err == nil {
		t.Fatal("target without adoptRoots was accepted")
	}
}

func TestAttachControlRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	req := core.AttachRequest{ID: core.NewID(), At: time.Now().UnixMilli(), User: "ou_dm", Native: "native", HolderPID: 7}
	path, err := writeAttachRequest(dataDir, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	go func() {
		resultPath := filepath.Join(dataDir, "control", "attach", req.ID+".result.json")
		data, _ := json.Marshal(core.AttachResult{OK: true, State: core.AdoptionOpen, Room: "room", ThreadID: "omt_x"})
		_ = os.WriteFile(resultPath, data, 0600)
	}()
	result, confirmed, err := waitAttachResult(context.Background(), dataDir, req.ID)
	if err != nil || !confirmed || !result.OK || result.ThreadID != "omt_x" {
		t.Fatalf("result: %+v %v %v", result, confirmed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("request file was not cleaned up")
	}
}

type fakeAttachGateway struct {
	mu      sync.Mutex
	request core.AttachRequest
	result  core.AttachResult
}

func (g *fakeAttachGateway) Attach(_ context.Context, req core.AttachRequest) core.AttachResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.request = req
	return g.result
}

func (g *fakeAttachGateway) seen() core.AttachRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.request
}

func TestWatchAttachProcessesRequestAndWritesResult(t *testing.T) {
	dataDir := t.TempDir()
	req := core.AttachRequest{ID: core.NewID(), At: time.Now().UnixMilli(), User: "ou_dm", Native: "native"}
	if _, err := writeAttachRequest(dataDir, req); err != nil {
		t.Fatal(err)
	}
	gateway := &fakeAttachGateway{result: core.AttachResult{OK: true, State: core.AdoptionOpen, ThreadID: "omt_x"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchAttach(ctx, gateway, dataDir, nil)
	resultPath := filepath.Join(dataDir, "control", "attach", req.ID+".result.json")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(resultPath); err == nil {
			var result core.AttachResult
			if err := json.Unmarshal(data, &result); err != nil || !result.OK || result.ThreadID != "omt_x" {
				t.Fatalf("result: %+v %v", result, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher did not write a result")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if seen := gateway.seen(); seen.ID != req.ID || seen.Native != "native" {
		t.Fatalf("gateway request: %+v", gateway.request)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "control", "attach", req.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("request file was not consumed")
	}
}

func TestWatchAttachRecoversInterruptedProcessingAsUncertain(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "control", "attach")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := core.NewID()
	if err := os.WriteFile(filepath.Join(dir, id+".json.processing"), []byte(`{"id":"`+id+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	gateway := &fakeAttachGateway{result: core.AttachResult{OK: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchAttach(ctx, gateway, dataDir, nil)
	resultPath := filepath.Join(dir, id+".result.json")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(resultPath); err == nil {
			var result core.AttachResult
			if err := json.Unmarshal(data, &result); err != nil || result.State != core.AdoptionUncertain {
				t.Fatalf("recovered result: %+v %v", result, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("interrupted request was not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gateway.seen().ID != "" {
		t.Fatal("interrupted request was replayed")
	}
}

func TestRunAttachEndToEnd(t *testing.T) {
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := attachConfig(t, work)
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	nativeID := "11111111-2222-3333-4444-555555555555"
	project := filepath.Join(configDir, "projects", native.EncodeProjectDir(work))
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	cwdJSON, _ := json.Marshal(work)
	if err := os.WriteFile(filepath.Join(project, nativeID+".jsonl"), []byte(`{"cwd":`+string(cwdJSON)+`}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := workspace.Lock(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		dir := filepath.Join(c.DataDir, "control", "attach")
		deadline := time.Now().Add(5 * time.Second)
		for {
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".result.json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					continue
				}
				var req core.AttachRequest
				if json.Unmarshal(data, &req) != nil {
					continue
				}
				result, _ := json.Marshal(core.AttachResult{OK: true, State: core.AdoptionOpen, ChatID: "oc_dm", RootMessageID: "om_root", ThreadID: "omt_x"})
				_ = os.WriteFile(filepath.Join(dir, req.ID+".result.json"), result, 0600)
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	var out bytes.Buffer
	err = runAttach(context.Background(), c, Options{
		AttachUser: "ou_dm", AttachNative: nativeID, AttachHolderSet: true,
	}, &out)
	<-done
	if err != nil {
		t.Fatalf("run attach: %v; output: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "om_root") || !strings.Contains(out.String(), "omt_x") {
		t.Fatalf("output: %s", out.String())
	}
}
