package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func TestApprovalPolicyChecksWarnWithoutOwners(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.Projects = []core.Project{{ID: "team-tools", Maintainers: []string{"ou_owner"}, Approvals: core.ProjectApprovals{ForbidSelfApproval: true}}}
	checks := approvalPolicyChecks(c)
	if len(checks) != 1 || !checks[0].OK || !strings.Contains(checks[0].Detail, "Warning") {
		t.Fatalf("approval policy hint = %#v", checks)
	}
	c.Projects[0].Owners = []string{"ou_owner"}
	if checks := approvalPolicyChecks(c); len(checks) != 0 {
		t.Fatalf("owners did not suppress hint: %#v", checks)
	}
	report := RunDoctor(context.Background(), c, false, false)
	found := false
	for _, check := range report.Checks {
		found = found || check.Name == "project team-tools approval policy"
	}
	if found {
		t.Fatal("doctor reported a hint after owners were configured")
	}
}

func TestActionApprovalChecks(t *testing.T) {
	c := config.Defaults()
	c.Projects = []core.Project{
		{ID: "team-tools", Maintainers: []string{"ou_owner"}, Actions: []core.ActionSpec{
			{ID: "build", Approval: "none"},
			{ID: "read", Approval: "none", ReadOnly: true},
			{ID: "ota", Approval: "open"},
			{ID: "openread", Approval: "open", ReadOnly: true},
		}},
		{ID: "no-owner", Actions: []core.ActionSpec{{ID: "deploy", Approval: "none"}}},
		{ID: "bad", Maintainers: []string{"ou_owner"}, Actions: []core.ActionSpec{{ID: "other", Approval: "sometimes"}}},
	}
	checks := actionApprovalChecks(c)
	byName := map[string]Diagnostic{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if check, ok := byName["project team-tools action read approval"]; !ok || !check.OK || !strings.Contains(check.Detail, "redundant") {
		t.Fatalf("readOnly warning = %#v", check)
	}
	if check, ok := byName["project team-tools action ota approval"]; !ok || !check.OK || !strings.Contains(check.Detail, "Info:") || !strings.Contains(check.Detail, "bot senders are rejected") || !strings.Contains(check.Detail, "action itself must enforce authorization") {
		t.Fatalf("open info = %#v", check)
	}
	if check, ok := byName["project team-tools action openread approval"]; !ok || !check.OK || !strings.Contains(check.Detail, "redundant") {
		t.Fatalf("readOnly open warning = %#v", check)
	}
	if check, ok := byName["project no-owner approval:none maintainers"]; !ok || check.OK {
		t.Fatalf("missing maintainer check = %#v", check)
	}
	if check, ok := byName["project bad action other approval"]; !ok || check.OK || !strings.Contains(check.Detail, "card, none or open") {
		t.Fatalf("invalid approval check = %#v", check)
	}
	for name := range byName {
		if name == "project team-tools action build approval" {
			t.Fatalf("valid direct action produced a diagnostic: %#v", byName[name])
		}
	}
}

func TestDeploymentChecksRejectPlaceholderApprovers(t *testing.T) {
	base := config.Defaults()
	base.TenantID, base.AppID = "tenant_local", "cli_local"
	base.Model.BaseURL, base.Model.Model, base.Model.SmallModel = "https://model.internal", "model", "model"
	base.Projects = []core.Project{{
		ID: "team-tools", Maintainers: []string{"ou_owner"},
		Actions: []core.ActionSpec{{ID: "publish", Approvers: []string{"ou_example_approver"}}},
	}}
	checks := deploymentChecks(base)
	found := false
	for _, check := range checks {
		if check.Name == "configuration values" {
			found = true
			if check.OK || !strings.Contains(check.Detail, "projects[0].actions[publish].approvers[0]") {
				t.Fatalf("placeholder approver check = %#v", check)
			}
		}
	}
	if !found {
		t.Fatal("configuration values check missing")
	}
	base.Projects[0].Actions[0].Approvers = []string{"ou_team_approver"}
	for _, check := range deploymentChecks(base) {
		if check.Name == "configuration values" && !check.OK {
			t.Fatalf("valid approver failed configuration values: %#v", check)
		}
	}
}

func TestConfigExplainApprovalPolicyView(t *testing.T) {
	dir := t.TempDir()
	path := writeInstallConfig(t, dir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	project := value["projects"].([]any)[0].(map[string]any)
	project["owners"] = []any{"ou_example_owner"}
	project["approvals"] = map[string]any{"forbidSelfApproval": true}
	value["dms"] = append(value["dms"].([]any), map[string]any{"userId": "ou_example_owner", "projectId": "p"}, map[string]any{"userId": "ou_viewer", "projectId": "p"})
	data, _ = json.Marshal(value)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	o, err := Parse([]string{"config", "explain", "--config", path, "--chat", "private-chat", "--user", "ou_example_owner", "--chat-type", "p2p", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runConfigExplain(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["ownerCount"] != float64(1) || view["forbidSelfApproval"] != true || view["isOwner"] != true {
		t.Fatalf("approval policy view = %#v", view)
	}
	o.ExplainUser = "ou_viewer"
	out.Reset()
	if err := runConfigExplain(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["isOwner"] != false {
		t.Fatalf("non-owner view = %#v", view)
	}
	o.JSON = false
	out.Reset()
	if err := runConfigExplain(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Owners: 1", "Forbid self approval: true", "User is owner: false"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("text explain missing %q: %s", want, out.String())
		}
	}
}
