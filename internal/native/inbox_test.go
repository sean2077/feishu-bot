package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInboxFrames(t *testing.T) {
	wire, err := inboxFrames("token-value", "hello\nworld")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(wire), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frames = %d", len(lines))
	}
	var auth map[string]any
	if err = json.Unmarshal([]byte(lines[0]), &auth); err != nil || auth["type"] != "auth" || auth["token"] != "token-value" {
		t.Fatalf("auth frame: %v %v", auth, err)
	}
	var user struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err = json.Unmarshal([]byte(lines[1]), &user); err != nil || user.Type != "user" || user.Message.Role != "user" || user.Message.Content != "hello\nworld" {
		t.Fatalf("user frame: %+v %v", user, err)
	}
}

func TestInboxFramesRejectNUL(t *testing.T) {
	if _, err := inboxFrames("token", "bad\x00text"); err == nil {
		t.Fatal("NUL text was accepted")
	}
}

func TestSendInboxValidatesTarget(t *testing.T) {
	if err := SendInbox(context.Background(), "", "", "text"); err == nil {
		t.Fatal("empty socket was accepted")
	}
	if runtime.GOOS == "windows" {
		if err := SendInbox(context.Background(), `\\.\pipe\missing`, "", "text"); err == nil {
			t.Fatal("missing Windows token was accepted")
		}
	}
	if err := SendInbox(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), "token", "text"); err == nil {
		t.Fatal("missing inbox socket was accepted")
	}
}

func TestSendInboxErrorsDoNotContainToken(t *testing.T) {
	err := SendInbox(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), "secret-token-value", "text")
	if err == nil {
		t.Fatal("missing socket was accepted")
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("error leaked the token: %v", err)
	}
}
