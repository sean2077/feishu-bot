package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func hookDenied(t *testing.T, w *worker, tool string, input args, cwd string) bool {
	t.Helper()
	request := args{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input}
	if cwd != "" {
		request["cwd"] = cwd
	}
	result, err := w.hook(context.Background(), "tool", request)
	if err != nil {
		t.Fatal(err)
	}
	return object(result["hookSpecificOutput"])["permissionDecision"] == "deny"
}

func windowsScopeFixture(t *testing.T, mode string) (*worker, string, string) {
	t.Helper()
	base := t.TempDir()
	rooms := filepath.Join(base, "rooms")
	room := filepath.Join(rooms, "room")
	work := filepath.Join(room, "workspace")
	other := filepath.Join(rooms, "other", "inbound")
	for _, p := range []string{work, filepath.Join(room, "home"), filepath.Join(room, "inbound"), other} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	w := &worker{workspace: core.Workspace{Root: room, Work: work, Home: filepath.Join(room, "home"), Inbound: filepath.Join(room, "inbound"), NativeConfig: mode}, events: make(chan core.WorkerEvent, 256), eventsDone: make(chan struct{})}
	return w, work, other
}

func TestWindowsDeviceAndUNCSearchPathsAreDenied(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path forms")
	}
	for _, mode := range []string{"isolated", "inherit"} {
		w, work, other := windowsScopeFixture(t, mode)
		volume := filepath.VolumeName(other)
		for _, path := range []string{`\?\` + other, `\.\` + other, `\localhost\` + volume[:1] + `$` + other[2:], `\?\UNC\localhost\` + volume[:1] + `$` + other[2:], "//localhost/" + volume[:1] + "$" + filepath.ToSlash(other[2:])} {
			if !hookDenied(t, w, "Grep", args{"path": path}, "") {
				t.Fatalf("%s: device/UNC path allowed: %s", mode, path)
			}
			if !hookDenied(t, w, "Glob", args{"pattern": path + `\*`}, "") {
				t.Fatalf("%s: device/UNC pattern allowed: %s", mode, path)
			}
		}
		if hookDenied(t, w, "Grep", args{"path": work}, "") {
			t.Fatalf("%s: ordinary drive path denied", mode)
		}
	}
}

func TestJunctionToAnotherRoomIsDenied(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("directory junctions")
	}
	for _, mode := range []string{"isolated", "inherit"} {
		w, work, other := windowsScopeFixture(t, mode)
		link := filepath.Join(work, "j")
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, other).CombinedOutput(); err != nil {
			t.Skipf("cannot create junction: %v %s", err, out)
		}
		if !hookDenied(t, w, "Grep", args{"path": "j"}, "") || !hookDenied(t, w, "Read", args{"file_path": filepath.Join(link, "x.txt")}, "") {
			t.Fatalf("%s: junction into another room allowed", mode)
		}
	}
}

func TestUnusableSessionDirectoryDeniesRelativeSearches(t *testing.T) {
	w, work, _ := windowsScopeFixture(t, "inherit")
	odd := filepath.Join(work, "a ")
	if !hookDenied(t, w, "Grep", args{"path": "src"}, odd) {
		t.Fatal("relative search allowed with an unresolvable session directory")
	}
	if hookDenied(t, w, "Grep", args{"path": work}, odd) {
		t.Fatal("absolute search denied with an unresolvable session directory")
	}
	if hookDenied(t, w, "Grep", args{"path": "src"}, work) {
		t.Fatal("ordinary relative search denied")
	}
}

func TestGlobRootMatchesClaudeCodeSplit(t *testing.T) {
	for _, mode := range []string{"isolated", "inherit"} {
		w, work, other := windowsScopeFixture(t, mode)
		otherRoom := filepath.Dir(other)
		mine := filepath.Base(filepath.Dir(work))
		up := filepath.Join("a", "..", "..", "..", mine, "workspace", "x")
		for _, pattern := range []string{
			filepath.Join(other, "{"+up+",**}", "*"),
			filepath.Join(otherRoom, "!zzz"),
			filepath.Join(other, "report.pdf"),
		} {
			if !hookDenied(t, w, "Glob", args{"pattern": pattern}, "") {
				t.Fatalf("%s: glob escaped its checked root: %s", mode, pattern)
			}
		}
		for _, pattern := range []string{filepath.Join(work, "**", "*.go"), filepath.Join(work, "src", "{a,b}", "*.ts")} {
			if hookDenied(t, w, "Glob", args{"pattern": pattern}, "") {
				t.Fatalf("%s: ordinary absolute glob denied: %s", mode, pattern)
			}
		}
		if hookDenied(t, w, "Glob", args{"pattern": "**/*.go"}, work) || hookDenied(t, w, "Grep", args{"pattern": "TODO"}, work) {
			t.Fatalf("%s: path-less search in the workspace denied", mode)
		}
		if !hookDenied(t, w, "Glob", args{"pattern": "**/*"}, other) || !hookDenied(t, w, "Grep", args{"pattern": "x"}, other) {
			t.Fatalf("%s: path-less search from another room's directory allowed", mode)
		}
	}
}
