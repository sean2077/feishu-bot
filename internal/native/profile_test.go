package native

import (
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func TestAutoModeForcesLocalClassifierWithoutChangingTrafficPolicy(t *testing.T) {
	base := core.Config{Model: core.ModelConfig{Model: "default-model", SmallModel: "default-small"}}
	auto := modelEnvironment(base, "fixture-token", nil, true)
	if auto["CLAUDE_CODE_AUTO_MODE_SERVER"] != "0" {
		t.Fatalf("auto mode did not force the local classifier: %#v", auto)
	}
	if auto["ANTHROPIC_API_KEY"] != "" || auto["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Fatalf("auto mode changed credential or traffic policy: %#v", auto)
	}
	if _, ok := auto["DISABLE_TELEMETRY"]; ok {
		t.Fatalf("auto mode left telemetry enabled unexpectedly: %#v", auto)
	}
	normal := modelEnvironment(base, "fixture-token", nil, false)
	if _, ok := normal["CLAUDE_CODE_AUTO_MODE_SERVER"]; ok {
		t.Fatalf("non-auto project received auto classifier mode: %#v", normal)
	}
}

func TestNamedProfileUsesNativeStartupAndKeepsResume(t *testing.T) {
	base := core.Config{Model: core.ModelConfig{Model: "default-model", SmallModel: "default-small"}, ModelProfiles: map[string]core.ModelProfile{"careful": {Model: "selected-model", SmallModel: "selected-small", Effort: "high"}}}
	selected, err := config.ResolveSessionModel(base, "careful")
	if err != nil {
		t.Fatal(err)
	}
	env := modelEnvironment(selected, "fixture-token", nil, false)
	if env["ANTHROPIC_MODEL"] != "selected-model" || env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "selected-small" || env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Fatal("profile not passed through documented native environment")
	}
	args := strings.Join(launchArgs(selected, core.Project{}, core.Session{CCID: "existing-native-session"}, core.Workspace{}, ""), " ")
	if !strings.Contains(args, "--model selected-model") || !strings.Contains(args, "--resume existing-native-session") {
		t.Fatal("native startup did not preserve model and resume arguments", args)
	}
	if _, ok := modelEnvironment(base, "fixture-token", nil, false)["CLAUDE_CODE_EFFORT_LEVEL"]; ok {
		t.Fatal("profile effort leaked into another worker's defaults")
	}
}
