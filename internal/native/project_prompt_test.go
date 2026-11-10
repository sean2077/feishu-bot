package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func projectPromptWorkspace(t *testing.T) core.Workspace {
	t.Helper()
	root := t.TempDir()
	w := core.Workspace{Root: root, Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Inbound: filepath.Join(root, "inbound"), NativeConfig: "isolated"}
	for _, dir := range []string{w.Work, w.Home, w.Inbound} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func TestLoadProjectPromptInlineAndFile(t *testing.T) {
	w := projectPromptWorkspace(t)
	p := core.Project{ID: "p", AppendSystemPrompt: "inline instructions"}
	if text, reason := loadProjectPrompt(p, w); text != "inline instructions" || reason != "" {
		t.Fatalf("inline prompt = %q %q", text, reason)
	}

	path := filepath.Join(t.TempDir(), "team.prompt")
	if err := os.WriteFile(path, []byte("file instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	p = core.Project{ID: "p", AppendSystemPromptFile: path}
	if text, reason := loadProjectPrompt(p, w); text != "file instructions" || reason != "" {
		t.Fatalf("file prompt = %q %q", text, reason)
	}
	if err := os.WriteFile(path, []byte("updated instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if text, reason := loadProjectPrompt(p, w); text != "updated instructions" || reason != "" {
		t.Fatalf("updated prompt = %q %q", text, reason)
	}
}

func TestLoadProjectPromptFailuresAreClassified(t *testing.T) {
	w := projectPromptWorkspace(t)
	base := t.TempDir()
	cases := []struct {
		name   string
		path   string
		body   []byte
		reason string
	}{
		{"missing", filepath.Join(base, "missing.prompt"), nil, "missing"},
		{"too large", filepath.Join(base, "large.prompt"), []byte(strings.Repeat("x", 16<<10+1)), "too_large"},
		{"invalid utf8", filepath.Join(base, "bad.prompt"), []byte{0xff, 0xfe}, "invalid_utf8"},
		{"nul", filepath.Join(base, "nul.prompt"), []byte("x\x00y"), "nul"},
		{"empty", filepath.Join(base, "empty.prompt"), []byte{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.body != nil {
				if err := os.WriteFile(tc.path, tc.body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			p := core.Project{ID: "p", AppendSystemPromptFile: tc.path}
			text, reason := loadProjectPrompt(p, w)
			if reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
			if tc.reason == "" && text != "" {
				t.Fatalf("unexpected prompt %q", text)
			}
		})
	}

	if _, reason := loadProjectPrompt(core.Project{ID: "p", AppendSystemPrompt: string([]byte{0xff})}, w); reason != "invalid_utf8" {
		t.Fatalf("invalid inline prompt reason = %q", reason)
	}
	for name, path := range map[string]string{
		"inside home": filepath.Join(w.Home, "prompt.txt"),
		"inside root": filepath.Join(w.Root, "prompt.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, reason := loadProjectPrompt(core.Project{ID: "p", AppendSystemPromptFile: path}, w); reason != "path" {
				t.Fatalf("protected path reason = %q", reason)
			}
		})
	}
}

func TestInitializationAppendsProjectPromptAfterBridgeAndActions(t *testing.T) {
	p := core.Project{ID: "p", Actions: []core.ActionSpec{{ID: "read", Description: "read", ReadOnly: true}}}
	prompt := text(initialization(p, 300000, "PROJECT_MARKER")["appendSystemPrompt"])
	if !strings.Contains(prompt, "Use project_action") || !strings.Contains(prompt, "Project instructions:\nPROJECT_MARKER") {
		t.Fatalf("initialization prompt = %q", prompt)
	}
	if strings.Index(prompt, "Use project_action") > strings.Index(prompt, "Project instructions:") || strings.Index(prompt, "Project instructions:") > strings.Index(prompt, "PROJECT_MARKER") {
		t.Fatalf("project prompt order = %q", prompt)
	}
	if strings.Contains(text(initialization(core.Project{ID: "p"}, 300000)["appendSystemPrompt"]), "Project instructions:") {
		t.Fatal("empty project prompt added a heading")
	}
}

func TestProjectPromptReloadsOnNextNativeProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team.prompt")
	if err := os.WriteFile(path, []byte("first prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.config.Projects[0].AppendSystemPromptFile = path
	f.start()
	if got := f.worker.(*worker).projectPrompt; got != "first prompt" {
		t.Fatalf("first prompt = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := f.worker.Stop(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := os.WriteFile(path, []byte("second prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	f.start()
	if got := f.worker.(*worker).projectPrompt; got != "second prompt" {
		t.Fatalf("reloaded prompt = %q", got)
	}
}

func TestProjectPromptUnavailableIsFailOpenAndAuditedOnce(t *testing.T) {
	f := newFixture(t)
	f.config.Projects[0].AppendSystemPromptFile = filepath.Join(t.TempDir(), "missing.prompt")
	f.start()
	event := f.event("project_prompt_unavailable")
	if event.Reason != "missing" {
		t.Fatalf("prompt event = %+v", event)
	}
	f.mu.Lock()
	count := 0
	for _, e := range f.all {
		if e.Type == "project_prompt_unavailable" {
			count++
		}
	}
	f.mu.Unlock()
	if count != 1 {
		t.Fatalf("project prompt audit events = %d", count)
	}
	// The process still starts and accepts input.
	f.send("still works")
	_ = f.event("ready")
}

func TestInteractiveIncludesProjectPrompt(t *testing.T) {
	f := newFixture(t)
	f.config.Projects[0].AppendSystemPrompt = "PROMPT_MARKER"
	s := f.session
	s.CCID = core.NewID()
	s.RoomKey = "room"
	cmd, cleanup, err := interactiveCommand(f.config, core.Room{Key: "room", ProjectID: "p"}, s, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	args := strings.Join(cmd.Args, " ")
	index := strings.Index(args, "--append-system-prompt")
	if index < 0 || !strings.Contains(args[index:], "Project instructions:") || !strings.Contains(args[index:], "PROMPT_MARKER") {
		t.Fatalf("interactive prompt args = %s", args)
	}
}
