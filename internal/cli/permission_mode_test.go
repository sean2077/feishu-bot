package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestConfigExplainShowsAutoModeClassifier(t *testing.T) {
	c := core.Config{Projects: []core.Project{{ID: "auto", Maintainers: []string{"owner"}, PermissionMode: "auto"}}, Groups: []core.Group{{ChatID: "group", ProjectID: "auto", Members: []string{"member"}}}}
	v := explainRoute(c, Options{ExplainChat: "group", ExplainUser: "member", ChatType: "group", Mentioned: true})
	auto, _ := v["autoMode"].(map[string]any)
	if fmt.Sprint(auto["classifierServerEnv"]) != "CLAUDE_CODE_AUTO_MODE_SERVER=0" || fmt.Sprint(auto["classifierModel"]) != "claude-sonnet-5" || fmt.Sprint(auto["availability"]) != "runtime_not_checked" {
		t.Fatalf("auto explain = %#v", v["autoMode"])
	}
}

func TestPermissionModeDoctorChecks(t *testing.T) {
	c := core.Config{Projects: []core.Project{
		{ID: "auto-project", PermissionMode: "auto"},
		{ID: "default-project"},
	}}
	checks := permissionModeChecks(c)
	if len(checks) != 2 || !checks[0].OK || !checks[1].OK {
		t.Fatalf("permission mode checks = %+v", checks)
	}
	if !strings.Contains(checks[0].Detail, "auto") || !strings.Contains(checks[0].Detail, "claude-sonnet-5") || !strings.Contains(checks[0].Detail, "not_checked") {
		t.Fatalf("auto detail = %q", checks[0].Detail)
	}
	if !strings.Contains(checks[1].Detail, "acceptEdits") {
		t.Fatalf("default detail = %q", checks[1].Detail)
	}
}
