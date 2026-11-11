package cli

import (
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestConfigExplainShowsProjectModelProfile(t *testing.T) {
	c := core.Config{Projects: []core.Project{{ID: "project", Maintainers: []string{"owner"}, ModelProfile: "fast", PermissionMode: "default"}}, Groups: []core.Group{{ChatID: "group", ProjectID: "project", Members: []string{"member"}}}}
	view := explainRoute(c, Options{ExplainChat: "group", ExplainUser: "member", ChatType: "group", Mentioned: true})
	if view["modelProfile"] != "fast" {
		t.Fatalf("config explain model profile = %v", view["modelProfile"])
	}
}

func TestProjectModelProfileDoctorChecks(t *testing.T) {
	c := core.Config{Projects: []core.Project{
		{ID: "inherited"},
		{ID: "named", ModelProfile: "fast"},
		{ID: "deployment", ModelProfile: "default"},
	}}
	checks := projectModelProfileChecks(c)
	if len(checks) != 3 {
		t.Fatalf("model profile checks = %+v", checks)
	}
	for _, check := range checks {
		if !check.OK {
			t.Fatalf("valid project model profile reported as failure: %+v", check)
		}
		if strings.Contains(check.Detail, "token") || strings.Contains(check.Detail, "secret") {
			t.Fatalf("model profile detail leaked credentials: %q", check.Detail)
		}
	}
	if !strings.Contains(checks[0].Detail, "deployment model") {
		t.Fatalf("unconfigured project detail = %q", checks[0].Detail)
	}
	if !strings.Contains(checks[1].Detail, "fast") {
		t.Fatalf("named project detail = %q", checks[1].Detail)
	}
	if !strings.Contains(checks[2].Detail, "default") {
		t.Fatalf("deployment project detail = %q", checks[2].Detail)
	}
}
