package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendSystemPromptConfig(t *testing.T) {
	base := t.TempDir()
	v := fixture(t)
	projectConfig(v)["appendSystemPrompt"] = "Use the team knowledge base."
	data, _ := json.Marshal(v)
	c, err := Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].AppendSystemPrompt != "Use the team knowledge base." {
		t.Fatalf("inline prompt = %q", c.Projects[0].AppendSystemPrompt)
	}

	v = fixture(t)
	projectConfig(v)["appendSystemPromptFile"] = "prompts/team.prompt"
	data, _ = json.Marshal(v)
	c, err = Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].AppendSystemPromptFile != filepath.Join(base, "prompts", "team.prompt") {
		t.Fatalf("prompt file = %q", c.Projects[0].AppendSystemPromptFile)
	}

	for name, prompt := range map[string]any{
		"both":      map[string]any{"appendSystemPrompt": "x", "appendSystemPromptFile": "p"},
		"too large": map[string]any{"appendSystemPrompt": strings.Repeat("x", 16<<10+1)},
		"nul":       map[string]any{"appendSystemPrompt": "x\x00y"},
	} {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			for key, value := range prompt.(map[string]any) {
				projectConfig(v)[key] = value
			}
			data, _ := json.Marshal(v)
			if _, err := Parse(data, base); err == nil {
				t.Fatal("invalid prompt configuration accepted")
			}
		})
	}
}

func TestAppendSystemPromptFileCannotBeInRoomTree(t *testing.T) {
	v := fixture(t)
	projectConfig(v)["appendSystemPromptFile"] = "./state/rooms/room/prompt.txt"
	data, _ := json.Marshal(v)
	if _, err := Parse(data, t.TempDir()); err == nil {
		t.Fatal("prompt file inside dataDir room tree accepted")
	}
}