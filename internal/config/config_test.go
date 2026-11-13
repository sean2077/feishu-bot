package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func fixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../examples/config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestExistingConfigurationDefaults(t *testing.T) {
	v := fixture(t)
	data, _ := json.Marshal(v)
	base := t.TempDir()
	c, err := Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime.MaxWorkers != 4 || c.Runtime.IdleMS != 900000 || c.Limits.BackgroundMessages != 0 || c.Limits.ImageBatchMS != 0 {
		t.Fatalf("changed defaults: %+v %+v", c.Runtime, c.Limits)
	}
	if c.Projects[0].PermissionMode != "acceptEdits" || c.Projects[0].NativeConfig != "isolated" || c.DataDir != filepath.Join(base, "state") {
		t.Fatal("existing configuration semantics changed")
	}
	if len(c.Groups) != 1 || c.Groups[0].MentionOptional {
		t.Fatal("mentionOptional must default to false")
	}
	if c.Groups[0].AllowAllMembers {
		t.Fatal("allowAllMembers must default to false")
	}
	if c.Groups[0].AllowOtherBots {
		t.Fatal("allowOtherBots must default to false")
	}
}
func TestAllowOtherBotsGroupConfig(t *testing.T) {
	v := fixture(t)
	v["groups"].([]any)[0].(map[string]any)["allowOtherBots"] = true
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Groups[0].AllowOtherBots {
		t.Fatal("allowOtherBots was not applied")
	}
	if r, ok := c.RoomFor("oc_example_group", ""); !ok || !r.AllowOtherBots {
		t.Fatalf("explicit room does not carry allowOtherBots: %+v", r)
	}
	group := v["groups"].([]any)[0].(map[string]any)
	group["otherBots"] = []any{map[string]any{"appId": "cli_helper", "name": "Helper", "openId": "ou_helper"}}
	data, _ = json.Marshal(v)
	c, err = Parse(data, t.TempDir())
	if err != nil || len(c.Groups[0].OtherBots) != 1 || c.Groups[0].OtherBots[0].OpenID != "ou_helper" {
		t.Fatalf("otherBots = %+v %v", c.Groups[0].OtherBots, err)
	}
	group["allowOtherBots"] = false
	data, _ = json.Marshal(v)
	if _, err = Parse(data, t.TempDir()); err == nil || !strings.Contains(err.Error(), "otherBots") {
		t.Fatalf("otherBots without allowOtherBots should fail: %v", err)
	}
	group["allowOtherBots"] = true
	group["otherBots"] = []any{map[string]any{"appId": "wrong", "name": "Helper"}}
	data, _ = json.Marshal(v)
	if _, err = Parse(data, t.TempDir()); err == nil || !strings.Contains(err.Error(), "appId") {
		t.Fatalf("invalid other bot appId should fail: %v", err)
	}
	group["otherBots"] = []any{map[string]any{"appId": "cli_helper", "name": "Helper", "openId": "wrong"}}
	data, _ = json.Marshal(v)
	if _, err = Parse(data, t.TempDir()); err == nil || !strings.Contains(err.Error(), "openId") {
		t.Fatalf("invalid other bot openId should fail: %v", err)
	}
	group["otherBots"] = []any{map[string]any{"appId": "cli_helper", "name": "Helper"}}
	v["groups"].([]any)[0].(map[string]any)["chatId"] = core.AnyGroupChatID
	data, _ = json.Marshal(v)
	if _, err = Parse(data, t.TempDir()); err == nil || !strings.Contains(err.Error(), "allowOtherBots") {
		t.Fatalf("wildcard allowOtherBots should fail: %v", err)
	}
}
func TestDefaultGroupBindingConfig(t *testing.T) {
	v := fixture(t)
	v["groups"] = []any{
		map[string]any{"chatId": core.AnyGroupChatID, "projectId": "assistant"},
		map[string]any{"chatId": "oc_named", "projectId": "assistant", "allowAllMembers": true},
	}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Groups) != 2 || c.Groups[0].ChatID != core.AnyGroupChatID || c.Groups[1].AllowAllMembers != true {
		t.Fatalf("default group binding: %+v", c.Groups)
	}
}
func TestMentionOptionalGroupConfig(t *testing.T) {
	v := fixture(t)
	v["groups"].([]any)[0].(map[string]any)["mentionOptional"] = true
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Groups[0].MentionOptional {
		t.Fatal("mentionOptional was not applied")
	}
}
func TestThreadSessionScopeGroupConfig(t *testing.T) {
	v := fixture(t)
	v["groups"] = []any{
		map[string]any{"chatId": core.AnyGroupChatID, "projectId": "assistant", "sessionScope": core.SessionScopeThread},
		map[string]any{"chatId": "oc_named", "projectId": "assistant", "sessionScope": core.SessionScopeChat},
	}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Groups[0].SessionScope != core.SessionScopeThread || c.Groups[1].SessionScope != core.SessionScopeChat {
		t.Fatalf("session scope: %+v", c.Groups)
	}
	if r, ok := c.RoomFor("oc_named", ""); !ok || r.SessionScope != core.SessionScopeChat {
		t.Fatalf("explicit chat scope: %+v", r)
	}
	if r, ok := c.RoomFor("oc_unlisted", ""); !ok || r.SessionScope != core.SessionScopeThread {
		t.Fatalf("wildcard thread scope: %+v", r)
	}
}
func TestEmbeddedExampleMatchesRepositoryExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Example {
		t.Fatal("examples/config.example.json and config.Example differ")
	}
}
func TestConfigRejectsInvalidAndUnknownValues(t *testing.T) {
	cases := map[string]func(map[string]any){
		"unknown":         func(v map[string]any) { v["limtis"] = map[string]any{} },
		"unknown nested":  func(v map[string]any) { v["runtime"].(map[string]any)["memory"] = 512 },
		"null limit":      func(v map[string]any) { v["limits"].(map[string]any)["approvalMs"] = nil },
		"fractional":      func(v map[string]any) { v["runtime"].(map[string]any)["idleMs"] = 0.5 },
		"unbounded queue": func(v map[string]any) { v["limits"].(map[string]any)["inboxPerRoom"] = 100000 },
		"url credentials": func(v map[string]any) { v["model"].(map[string]any)["baseUrl"] = "https://user:secret@example.com" },
		"header injection": func(v map[string]any) {
			v["model"].(map[string]any)["extraHeaders"] = map[string]any{"X-Token": "ok\r\nX: no"}
		},
		"wildcard maintainer": func(v map[string]any) { v["projects"].([]any)[0].(map[string]any)["maintainers"] = []string{"*"} },
		"wildcard member":     func(v map[string]any) { v["groups"].([]any)[0].(map[string]any)["members"] = []string{"*"} },
		"duplicate default group": func(v map[string]any) {
			g := v["groups"].([]any)
			g[0].(map[string]any)["chatId"] = core.AnyGroupChatID
			v["groups"] = append(g, map[string]any{"chatId": core.AnyGroupChatID, "projectId": "assistant"})
		},
		"allow all wrong type": func(v map[string]any) {
			v["groups"].([]any)[0].(map[string]any)["allowAllMembers"] = "true"
		},
		"duplicate group":    func(v map[string]any) { g := v["groups"].([]any); v["groups"] = append(g, g[0]) },
		"mention null":       func(v map[string]any) { v["groups"].([]any)[0].(map[string]any)["mentionOptional"] = nil },
		"mention wrong type": func(v map[string]any) { v["groups"].([]any)[0].(map[string]any)["mentionOptional"] = "true" },
		"mention casing":     func(v map[string]any) { v["groups"].([]any)[0].(map[string]any)["mentionoptional"] = true },
		"unknown session scope": func(v map[string]any) {
			v["groups"].([]any)[0].(map[string]any)["sessionScope"] = "user"
		},
		"session scope casing": func(v map[string]any) {
			v["groups"].([]any)[0].(map[string]any)["sessionScope"] = "Thread"
		},
		"session scope wrong type": func(v map[string]any) {
			v["groups"].([]any)[0].(map[string]any)["sessionScope"] = true
		},
		"cwd seed conflict": func(v map[string]any) {
			p := v["projects"].([]any)[0].(map[string]any)
			p["cwd"] = "existing"
			p["seedDir"] = "seed"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			change(v)
			data, _ := json.Marshal(v)
			if _, err := Parse(data, t.TempDir()); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}
func TestExplicitZeroAndLegacyAliases(t *testing.T) {
	v := fixture(t)
	v["runtime"].(map[string]any)["mode"] = "local"
	v["runtime"].(map[string]any)["idleMs"] = 0
	p := v["projects"].([]any)[0].(map[string]any)
	delete(p, "permissionMode")
	p["autoApproveWorkspace"] = false
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime.IdleMS != 0 || c.Projects[0].PermissionMode != "default" {
		t.Fatal("lost explicit legacy semantics")
	}
}
func TestHeadersResolvedOnlyFromExplicitVariables(t *testing.T) {
	v := fixture(t)
	v["model"].(map[string]any)["extraHeaders"] = map[string]any{"X-Deployment": "env:FEISHU_TEST_HEADER"}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FEISHU_TEST_HEADER", "")
	if _, err = ModelHeaders(c); err == nil {
		t.Fatal("accepted missing header")
	}
	t.Setenv("FEISHU_TEST_HEADER", "actual")
	h, err := ModelHeaders(c)
	if err != nil || h["X-Deployment"] != "actual" {
		t.Fatal(h, err)
	}
}

func TestExactConfigurationFieldNamesAndSafeErrors(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"permission casing": func(v map[string]any) {
			v["projects"].([]any)[0].(map[string]any)["permissionmode"] = "bypassPermissions"
		},
		"nested casing":           func(v map[string]any) { v["limits"].(map[string]any)["approvalMS"] = 1000 },
		"secret as unknown field": func(v map[string]any) { v["copied-private-key-not-for-diagnostics"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			mutate(v)
			data, _ := json.Marshal(v)
			_, err := Parse(data, t.TempDir())
			if err == nil {
				t.Fatal("accepted field the legacy schema rejected")
			}
			if strings.Contains(err.Error(), "copied-private-key") {
				t.Fatal("error exposed configuration content", err)
			}
		})
	}
	v := fixture(t)
	v["runtime"].(map[string]any)["maxWorkers"] = 987654321123456.0
	data, _ := json.Marshal(v)
	_, err := Parse(data, t.TempDir())
	if err == nil || strings.Contains(err.Error(), "987654321") {
		t.Fatal("invalid numeric value was exposed", err)
	}
}

func TestLegacyNullDefaultsAndIntegerNotation(t *testing.T) {
	v := fixture(t)
	for _, key := range []string{"runtime", "limits", "groups", "dms", "appSecretEnv"} {
		v[key] = nil
	}
	m := v["model"].(map[string]any)
	m["tokenEnv"] = nil
	m["extraHeaders"] = nil
	p := v["projects"].([]any)[0].(map[string]any)
	p["allowedTools"] = nil
	p["actions"] = nil
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal("rejected legacy null fallback", err)
	}
	if c.Runtime.MaxWorkers != 8 || c.Limits.ApprovalMS != 300000 || c.AppSecretEnv != "FEISHU_APP_SECRET" || c.Model.TokenEnv != "MODEL_AUTH_TOKEN" || len(c.Groups) != 0 || len(c.DMs) != 0 {
		t.Fatalf("legacy defaults changed: %+v", c)
	}
	v = fixture(t)
	data, _ = json.Marshal(v)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	raw["runtime"] = json.RawMessage(`{"maxWorkers":4.0,"idleMs":9e5}`)
	data, _ = json.Marshal(raw)
	c, err = Parse(data, t.TempDir())
	if err != nil || c.Runtime.MaxWorkers != 4 || c.Runtime.IdleMS != 900000 {
		t.Fatal("legacy integer representations rejected", c.Runtime, err)
	}
	for _, key := range []string{"permissionMode", "nativeConfig"} {
		v = fixture(t)
		v["projects"].([]any)[0].(map[string]any)[key] = nil
		data, _ = json.Marshal(v)
		if _, err = Parse(data, t.TempDir()); err == nil {
			t.Fatal("permission field null silently defaulted", key)
		}
	}
}

func TestLegacyActionWorkingDirectoryRemainsProcessRelative(t *testing.T) {
	v := fixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	processCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	p := v["projects"].([]any)[0].(map[string]any)
	p["actions"] = []any{map[string]any{"id": "status", "description": "Read project status", "executable": exe, "cwd": "operations", "args": nil, "env": nil, "vars": nil, "parameters": nil, "readOnly": true}}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	action := c.Projects[0].Actions[0]
	if action.CWD != filepath.Join(processCWD, "operations") || action.TimeoutMS != 60000 || action.Approval != "card" {
		t.Fatalf("action cwd/defaults changed: %+v", action)
	}
}

func TestActionApprovalValidatesModes(t *testing.T) {
	v := fixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := v["projects"].([]any)[0].(map[string]any)
	p["actions"] = []any{map[string]any{"id": "build", "description": "Build", "executable": exe, "approval": "open"}}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatalf("open action approval rejected: %v", err)
	}
	if c.Projects[0].Actions[0].Approval != "open" {
		t.Fatalf("open action approval = %q", c.Projects[0].Actions[0].Approval)
	}
	p["actions"] = []any{map[string]any{"id": "build", "description": "Build", "executable": exe, "approval": "sometimes"}}
	data, _ = json.Marshal(v)
	_, err = Parse(data, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "invalid action approval") {
		t.Fatalf("unknown action approval error = %v", err)
	}
}
