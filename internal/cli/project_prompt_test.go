package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestProjectPromptDoctorChecks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "team.prompt")
	if err := os.WriteFile(file, []byte("FILE_PROMPT_MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	c := core.Config{DataDir: dir, Projects: []core.Project{
		{ID: "inline", AppendSystemPrompt: "INLINE_PROMPT_MARKER"},
		{ID: "file", AppendSystemPromptFile: file},
		{ID: "missing", AppendSystemPromptFile: filepath.Join(dir, "missing.prompt")},
		{ID: "room", AppendSystemPromptFile: filepath.Join(dir, "rooms", "room", "prompt.txt")},
	}}
	checks := projectPromptChecks(c)
	if len(checks) != 4 {
		t.Fatalf("checks = %+v", checks)
	}
	for _, check := range checks {
		if strings.Contains(check.Detail, "_MARKER") {
			t.Fatalf("doctor leaked prompt content: %q", check.Detail)
		}
	}
	if !checks[0].OK || !strings.Contains(checks[0].Detail, "inline") || !strings.Contains(checks[0].Detail, "bytes") {
		t.Fatalf("inline check = %+v", checks[0])
	}
	if !checks[1].OK || !strings.Contains(checks[1].Detail, "file") || !strings.Contains(checks[1].Detail, "bytes") {
		t.Fatalf("file check = %+v", checks[1])
	}
	if checks[2].OK || !strings.Contains(checks[2].Detail, "missing") {
		t.Fatalf("missing check = %+v", checks[2])
	}
	if checks[3].OK || !strings.Contains(checks[3].Detail, "path") {
		t.Fatalf("room check = %+v", checks[3])
	}
}

func TestConfigExplainShowsProjectPromptMetadataOnly(t *testing.T) {
	c := core.Config{Projects: []core.Project{{ID: "p", Maintainers: []string{"owner"}, AppendSystemPrompt: "PROMPT_MARKER"}}, Groups: []core.Group{{ChatID: "group", ProjectID: "p", Members: []string{"member"}}}}
	view := explainRoute(c, Options{ExplainChat: "group", ExplainUser: "member", ChatType: "group", Mentioned: true})
	prompt, _ := view["appendSystemPrompt"].(map[string]any)
	if prompt["configured"] != true || prompt["source"] != "inline" || prompt["bytes"] != len("PROMPT_MARKER") {
		t.Fatalf("explain prompt = %#v", view["appendSystemPrompt"])
	}
	if strings.Contains(fmt.Sprint(view["appendSystemPrompt"]), "PROMPT_MARKER") {
		t.Fatal("config explain leaked prompt content")
	}
}

func TestConfigExplainShowsFilePromptBytesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team.prompt")
	if err := os.WriteFile(path, []byte("FILE_PROMPT_MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	c := core.Config{Projects: []core.Project{{ID: "p", Maintainers: []string{"owner"}, AppendSystemPromptFile: path}}, Groups: []core.Group{{ChatID: "group", ProjectID: "p", Members: []string{"member"}}}}
	view := explainRoute(c, Options{ExplainChat: "group", ExplainUser: "member", ChatType: "group", Mentioned: true})
	prompt, _ := view["appendSystemPrompt"].(map[string]any)
	if prompt["configured"] != true || prompt["source"] != "file" || prompt["bytes"] != len("FILE_PROMPT_MARKER") {
		t.Fatalf("explain file prompt = %#v", view["appendSystemPrompt"])
	}
	if strings.Contains(fmt.Sprint(view["appendSystemPrompt"]), "FILE_PROMPT_MARKER") {
		t.Fatal("config explain leaked file prompt content")
	}
}
