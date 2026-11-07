package native

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func FuzzOtherTopicFilePathsRemainDenied(f *testing.F) {
	room := filepath.Join(f.TempDir(), "rooms", "chat")
	work, home := filepath.Join(room, "workspace"), filepath.Join(room, "home")
	mine, other := filepath.Join(room, "topic-inbound", "mine"), filepath.Join(room, "topic-inbound", "other")
	for _, path := range []string{work, home, mine, other} {
		if err := os.MkdirAll(path, 0700); err != nil {
			f.Fatal(err)
		}
	}
	done := make(chan struct{})
	close(done)
	w := &worker{workspace: core.Workspace{Root: room, Work: work, Home: home, Inbound: mine, NativeConfig: "inherit"}, eventsDone: done}
	if _, err := w.fileScope(); err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{"upload.txt", "nested/report.pdf", "报告.txt", " spaced .txt", "nested/../upload.txt", "\ufefffile.txt"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, child string) {
		if len(child) > 512 {
			return
		}
		path := filepath.Join(other, child)
		if !core.Inside(other, path) {
			return
		}
		for _, tc := range []struct {
			tool  string
			input args
		}{
			{"Read", args{"file_path": path}},
			{"Grep", args{"path": path, "pattern": "."}},
			{"Glob", args{"path": path, "pattern": "*"}},
		} {
			out, err := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": tc.tool, "tool_input": tc.input})
			if err != nil || object(out["hookSpecificOutput"])["permissionDecision"] != "deny" {
				t.Fatalf("%s admitted a descendant of another topic: %q (%v)", tc.tool, child, err)
			}
		}
	})
}
