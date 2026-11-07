package native

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestInitAfterCancelStillReportsTheNativeSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &worker{ctx: ctx, cancel: cancel, events: make(chan core.WorkerEvent, 4), eventsDone: make(chan struct{})}
	w.handleFrame(args{"type": "system", "subtype": "init", "session_id": "native-after-stop"})
	w.handleFrame(args{"type": "result", "result": "ignored after stop"})
	select {
	case ev := <-w.events:
		if ev.Type != "ready" || ev.CCID != "native-after-stop" {
			t.Fatalf("unexpected event %+v", ev)
		}
	default:
		t.Fatal("native session report was dropped after cancellation")
	}
	select {
	case ev := <-w.events:
		t.Fatalf("non-init frame handled after cancellation: %+v", ev)
	default:
	}
}

func TestAmbiguousSearchPathsAreDenied(t *testing.T) {
	base := t.TempDir()
	room := filepath.Join(base, "rooms", "room")
	work := filepath.Join(room, "workspace")
	home := filepath.Join(room, "home")
	inbound := filepath.Join(room, "inbound")
	for _, p := range []string{work, home, inbound} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"isolated", "inherit"} {
		w := &worker{workspace: core.Workspace{Root: room, Work: work, Home: home, Inbound: inbound, NativeConfig: mode}, events: make(chan core.WorkerEvent, 16), eventsDone: make(chan struct{})}
		cases := []struct {
			tool  string
			input args
			deny  bool
		}{
			{"Grep", args{"path": " " + filepath.Join(base, "rooms")}, true},
			{"Grep", args{"path": filepath.Join(base, "rooms") + " "}, true},
			{"Grep", args{"path": "~/"}, true},
			{"Glob", args{"pattern": "~/**/*"}, true},
			{"Glob", args{"pattern": "../../*/inbound/*"}, true},
			{"Grep", args{"path": "src"}, false},
			{"Glob", args{"pattern": "**/*.go"}, false},
			{"Grep", args{"pattern": "TODO"}, false},
		}
		if runtime.GOOS == "windows" {
			cases = append(cases,
				struct {
					tool  string
					input args
					deny  bool
				}{"Grep", args{"path": `\Users\example\rooms`}, true},
				struct {
					tool  string
					input args
					deny  bool
				}{"Grep", args{"path": "/c/Users/example"}, true},
				struct {
					tool  string
					input args
					deny  bool
				}{"Glob", args{"pattern": "/Users/example/rooms/*/inbound/*"}, true},
			)
		}
		for _, tc := range cases {
			result, err := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": tc.tool, "tool_input": tc.input})
			if err != nil {
				t.Fatal(err)
			}
			if denied := object(result["hookSpecificOutput"])["permissionDecision"] == "deny"; denied != tc.deny {
				t.Fatalf("%s %s %v denied=%v want %v", mode, tc.tool, tc.input, denied, tc.deny)
			}
		}
	}
}
