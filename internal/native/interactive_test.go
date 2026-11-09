package native

import (
	"github.com/sean2077/feishu-bot/internal/core"
	"path/filepath"
	"strings"
	"testing"
)

func TestInteractiveLaunchKeepsIdentityProfileAndTerminalBoundary(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("FEISHU_INTERACTIVE_TEST_SECRET", "app-secret-must-not-reach-native")
	f.config.AppSecretEnv = "FEISHU_INTERACTIVE_TEST_SECRET"
	f.config.ModelProfiles = map[string]core.ModelProfile{"local": {Model: "selected-model", Effort: "high"}}
	t.Setenv("TEAM_API_TOKEN", "project-token")
	f.config.Projects[0].Env = map[string]string{"PROFILE": "team"}
	f.config.Projects[0].EnvPassthrough = []string{"TEAM_API_TOKEN"}
	s := f.session
	s.CCID = core.NewID()
	s.RoomKey = "room"
	s.Profile = "local"
	r := core.Room{Key: "room", ProjectID: "p"}
	cmd, cleanup, err := interactiveCommand(f.config, r, s, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--resume "+s.CCID) || !strings.Contains(args, "--model selected-model") || strings.Contains(args, "stream-json") || strings.Contains(args, "mcp__gateway__") || strings.Contains(args, "--await-initialize") {
		t.Fatal("interactive arguments changed native identity or enabled bridge protocol", args)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "TERM=xterm-256color") || !strings.Contains(env, "CLAUDE_CONFIG_DIR="+filepath.Join(f.workspace.Home, ".claude")) || !strings.Contains(env, "CLAUDE_CODE_EFFORT_LEVEL=high") || !strings.Contains(env, "PROFILE=team") || !strings.Contains(env, "TEAM_API_TOKEN=project-token") || strings.Contains(env, "app-secret-must-not-reach-native") || strings.Contains(env, "CLAUDE_CODE_ENTRYPOINT=") {
		t.Fatal("interactive environment boundary mismatch")
	}
	if cmd.Dir != f.workspace.Work {
		t.Fatal("interactive cwd changed")
	}
	s.CCID = ""
	if _, _, err := interactiveCommand(f.config, r, s, f.workspace); err == nil {
		t.Fatal("interactive launch silently created a new session")
	}
}
