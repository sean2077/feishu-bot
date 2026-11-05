package native

import (
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func TestNamedProfileUsesNativeStartupAndKeepsResume(t *testing.T) {
	base := core.Config{Model: core.ModelConfig{Model: "default-model", SmallModel: "default-small"}, ModelProfiles: map[string]core.ModelProfile{"careful": {Model: "selected-model", SmallModel: "selected-small", Effort: "high"}}}
	selected, err := config.ResolveSessionModel(base, "careful")
	if err != nil {
		t.Fatal(err)
	}
	env := modelEnvironment(selected, "fixture-token", nil)
	if env["ANTHROPIC_MODEL"] != "selected-model" || env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "selected-small" || env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Fatal("profile not passed through documented native environment")
	}
	args := strings.Join(launchArgs(selected, core.Project{}, core.Session{CCID: "existing-native-session"}, core.Workspace{}, ""), " ")
	if !strings.Contains(args, "--model selected-model") || !strings.Contains(args, "--resume existing-native-session") {
		t.Fatal("native startup did not preserve model and resume arguments", args)
	}
	if _, ok := modelEnvironment(base, "fixture-token", nil)["CLAUDE_CODE_EFFORT_LEVEL"]; ok {
		t.Fatal("profile effort leaked into another worker's defaults")
	}
}
