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

func TestInboxWriteOutcome(t *testing.T) {
	for _, tc := range []struct {
		n, total int
		err      error
		want     InboxOutcome
	}{
		{5, 5, nil, InboxSubmitted},
		{5, 5, context.Canceled, InboxSubmitted},
		{3, 5, context.DeadlineExceeded, InboxUncertain},
		{0, 5, context.DeadlineExceeded, InboxNotSent},
		{0, 5, nil, InboxNotSent},
	} {
		got, err := inboxWriteOutcome(tc.n, tc.total, tc.err)
		if got != tc.want {
			t.Fatalf("outcome(%d/%d, %v) = %v, want %v", tc.n, tc.total, tc.err, got, tc.want)
		}
		if got == InboxSubmitted && err != nil {
			t.Fatalf("submitted outcome returned error %v", err)
		}
		if got != InboxSubmitted && err == nil {
			t.Fatalf("unsent/uncertain outcome returned no error")
		}
	}
}

func TestSendInboxValidatesTarget(t *testing.T) {
	if outcome, err := SendInbox(context.Background(), "", "", "text"); err == nil || outcome != InboxNotSent {
		t.Fatalf("empty socket: %v %v", outcome, err)
	}
	if runtime.GOOS == "windows" {
		if outcome, err := SendInbox(context.Background(), `\\.\pipe\missing`, "", "text"); err == nil || outcome != InboxNotSent {
			t.Fatalf("missing Windows token: %v %v", outcome, err)
		}
	}
	if outcome, err := SendInbox(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), "token", "text"); err == nil || outcome != InboxNotSent {
		t.Fatalf("missing inbox socket: %v %v", outcome, err)
	}
}

func TestSendInboxErrorsDoNotContainToken(t *testing.T) {
	outcome, err := SendInbox(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), "secret-token-value", "text")
	if err == nil || outcome != InboxNotSent {
		t.Fatalf("missing socket: %v %v", outcome, err)
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("error leaked the token: %v", err)
	}
}
