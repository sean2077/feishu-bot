package config

import (
	"encoding/json"
	"os"
	"testing"
)

func TestApprovalPolicyConfig(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	v := fixture(t)
	p := projectConfig(v)
	p["owners"] = []any{"ou_example_owner"}
	p["approvals"] = map[string]any{"forbidSelfApproval": true}
	p["actions"] = []any{map[string]any{"id": "publish", "description": "Publish", "executable": exe, "parameters": map[string]any{}, "approvers": []any{"ou_example_approver"}}}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Projects[0].Owners) != 1 || c.Projects[0].Owners[0] != "ou_example_owner" {
		t.Fatalf("owners = %#v", c.Projects[0].Owners)
	}
	if !c.Projects[0].Approvals.ForbidSelfApproval {
		t.Fatal("forbidSelfApproval was not preserved")
	}
	if len(c.Projects[0].Actions) != 1 || len(c.Projects[0].Actions[0].Approvers) != 1 || c.Projects[0].Actions[0].Approvers[0] != "ou_example_approver" {
		t.Fatalf("approvers = %#v", c.Projects[0].Actions)
	}
}

func TestApprovalPolicyRejectsInvalidConfig(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"owner not maintainer": func(v map[string]any) {
			projectConfig(v)["owners"] = []any{"ou_example_other"}
		},
		"duplicate owner": func(v map[string]any) {
			projectConfig(v)["owners"] = []any{"ou_example_owner", "ou_example_owner"}
		},
		"wildcard owner": func(v map[string]any) {
			projectConfig(v)["owners"] = []any{"*"}
		},
		"empty owner": func(v map[string]any) {
			projectConfig(v)["owners"] = []any{""}
		},
		"null owners": func(v map[string]any) {
			projectConfig(v)["owners"] = nil
		},
		"unknown approvals field": func(v map[string]any) {
			projectConfig(v)["approvals"] = map[string]any{"other": true}
		},
		"forbid wrong type": func(v map[string]any) {
			projectConfig(v)["approvals"] = map[string]any{"forbidSelfApproval": "true"}
		},
		"duplicate approver": func(v map[string]any) {
			p := projectConfig(v)
			p["actions"] = []any{map[string]any{"id": "publish", "description": "Publish", "executable": exe, "parameters": map[string]any{}, "approvers": []any{"ou_example_approver", "ou_example_approver"}}}
		},
		"wildcard approver": func(v map[string]any) {
			p := projectConfig(v)
			p["actions"] = []any{map[string]any{"id": "publish", "description": "Publish", "executable": exe, "parameters": map[string]any{}, "approvers": []any{"*"}}}
		},
		"empty approver": func(v map[string]any) {
			p := projectConfig(v)
			p["actions"] = []any{map[string]any{"id": "publish", "description": "Publish", "executable": exe, "parameters": map[string]any{}, "approvers": []any{""}}}
		},
		"null approvers": func(v map[string]any) {
			p := projectConfig(v)
			p["actions"] = []any{map[string]any{"id": "publish", "description": "Publish", "executable": exe, "parameters": map[string]any{}, "approvers": nil}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			mutate(v)
			data, _ := json.Marshal(v)
			if _, err := Parse(data, t.TempDir()); err == nil {
				t.Fatal("accepted invalid approval policy")
			}
		})
	}
}
