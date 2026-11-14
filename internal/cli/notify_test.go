package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var coded *exitCodeError
	if !errors.As(err, &coded) {
		t.Fatalf("not an exit-code error: %v", err)
	}
	return coded.code
}

func TestParseNotifyAndTopicReply(t *testing.T) {
	o, err := Parse([]string{"notify", "--config", "c.json", "--summary-file", "-", "--title", "t", "--user", "ou_dm", "--native", "n", "--holder-pid", "7", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Command != "notify" || o.AttachSummaryFile != "-" || o.AttachTitle != "t" || o.AttachUser != "ou_dm" || o.AttachNative != "n" || o.AttachHolderPID != 7 || !o.AttachHolderSet || !o.JSON {
		t.Fatalf("notify options: %+v", o)
	}
	o, err = Parse([]string{"topic", "reply", "--config", "c.json", "--native", "n", "--message-id", "om_x", "--text-file", "-", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Command != "topic" || o.Subcommand != "reply" || o.AttachNative != "n" || o.TopicMessageID != "om_x" || o.TopicTextFile != "-" || !o.JSON {
		t.Fatalf("topic options: %+v", o)
	}
	for _, args := range [][]string{
		{"notify", "--summary-file"},
		{"topic", "unknown"},
		{"topic", "reply", "--text-file"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestNotifyResultExitCodes(t *testing.T) {
	cases := []struct {
		result core.NotifyResult
		code   int
	}{
		{core.NotifyResult{OK: true, State: core.AdoptionOpen}, 0},
		{core.NotifyResult{State: core.AdoptionPending}, 2},
		{core.NotifyResult{State: core.AdoptionUncertain}, 2},
		{core.NotifyResult{Error: &core.AdoptError{Code: core.NotifyErrorNotConfigured}}, 3},
		{core.NotifyResult{Error: &core.AdoptError{Code: core.NotifyErrorGatewayUnavailable}}, 4},
		{core.NotifyResult{Error: &core.AdoptError{Code: core.NotifyErrorInvalidRequest}}, 5},
		{core.NotifyResult{Error: &core.AdoptError{Code: core.NotifyErrorTransportFailure}}, 6},
	}
	for _, tc := range cases {
		if got := notifyResultExit(tc.result); got != tc.code {
			t.Fatalf("result %+v: got %d, want %d", tc.result, got, tc.code)
		}
	}
}

func TestRunNotifyNotConfigured(t *testing.T) {
	work := t.TempDir()
	c := attachConfig(t, work)
	c.DMs[0].AdoptRoots = nil
	var out bytes.Buffer
	err := runNotify(context.Background(), c, Options{AttachUser: "ou_dm", AttachNative: "native", AttachHolderSet: true, AttachSummaryFile: "-", JSON: true}, &out)
	if code := exitCodeOf(t, err); code != 3 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), core.NotifyErrorNotConfigured) {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRunNotifyGatewayUnavailable(t *testing.T) {
	work := t.TempDir()
	c := attachConfig(t, work)
	summary := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(summary, []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runNotify(context.Background(), c, Options{AttachUser: "ou_dm", AttachNative: "native", AttachHolderSet: true, AttachSummaryFile: summary, JSON: true}, &out)
	if code := exitCodeOf(t, err); code != 4 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), core.NotifyErrorGatewayUnavailable) {
		t.Fatalf("output: %s", out.String())
	}
}

func notifySuccessFixture(t *testing.T) (core.Config, string, string, string) {
	t.Helper()
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
	cwd, _ := json.Marshal(work)
	if err := os.WriteFile(filepath.Join(project, nativeID+".jsonl"), []byte(`{"cwd":`+string(cwd)+`}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(summary, []byte("task result"), 0600); err != nil {
		t.Fatal(err)
	}
	return c, nativeID, work, summary
}

func answerControlRequest(dataDir string, result any) error {
	dir := filepath.Join(dataDir, "control", "attach")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			var req struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &req) != nil {
				continue
			}
			encoded, _ := json.Marshal(result)
			_ = os.WriteFile(filepath.Join(dir, req.ID+".result.json"), encoded, 0600)
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("control request not observed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunNotifyEndToEnd(t *testing.T) {
	c, nativeID, _, summary := notifySuccessFixture(t)
	unlock, err := workspace.Lock(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	answer := make(chan error, 1)
	go func() {
		answer <- answerControlRequest(c.DataDir, core.NotifyResult{OK: true, State: core.AdoptionOpen, ChatID: "oc_dm", RootMessageID: "root", ThreadID: "thread", MessageID: "message", Created: true})
	}()
	var out bytes.Buffer
	err = runNotify(context.Background(), c, Options{AttachUser: "ou_dm", AttachNative: nativeID, AttachHolderSet: true, AttachSummaryFile: summary, JSON: true}, &out)
	if code := exitCodeOf(t, err); code != 0 {
		t.Fatalf("code = %d, output %s", code, out.String())
	}
	if e := <-answer; e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), `"created": true`) || !strings.Contains(out.String(), `"message_id": "message"`) {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRunTopicReplyEndToEnd(t *testing.T) {
	c, _, _, _ := notifySuccessFixture(t)
	text := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(text, []byte("answer"), 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := workspace.Lock(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	answer := make(chan error, 1)
	go func() {
		answer <- answerControlRequest(c.DataDir, core.TopicReplyResult{OK: true, MessageID: "reply-message"})
	}()
	var out bytes.Buffer
	err = runTopicReply(context.Background(), c, Options{AttachNative: "native", TopicMessageID: "om_x", TopicTextFile: text, JSON: true}, &out)
	if code := exitCodeOf(t, err); code != 0 {
		t.Fatalf("code = %d, output %s", code, out.String())
	}
	if e := <-answer; e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), `"message_id": "reply-message"`) {
		t.Fatalf("output: %s", out.String())
	}
}
