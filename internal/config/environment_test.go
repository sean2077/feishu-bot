package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func projectConfig(v map[string]any) map[string]any {
	return v["projects"].([]any)[0].(map[string]any)
}

func parseFixture(t *testing.T, v map[string]any) (string, error) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(data, t.TempDir())
	return string(data), err
}

func TestProjectEnvironmentParses(t *testing.T) {
	v := fixture(t)
	p := projectConfig(v)
	p["env"] = map[string]any{"PROFILE": "team", "TEAM_REGION": "eu-west"}
	p["envPassthrough"] = []any{"TEAM_API_TOKEN"}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].Env["PROFILE"] != "team" || c.Projects[0].Env["TEAM_REGION"] != "eu-west" {
		t.Fatalf("project env was not preserved: %#v", c.Projects[0].Env)
	}
	if len(c.Projects[0].EnvPassthrough) != 1 || c.Projects[0].EnvPassthrough[0] != "TEAM_API_TOKEN" {
		t.Fatalf("project passthrough was not preserved: %#v", c.Projects[0].EnvPassthrough)
	}
}

func TestProjectEnvironmentRejectsUnsafeDefinitions(t *testing.T) {
	secret := "literal-value-must-not-leak"
	base := func() map[string]any { return fixture(t) }
	cases := []struct {
		name     string
		mutate   func(map[string]any)
		variable string
		reason   string
	}{
		{"invalid name", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"1BAD": "x"}
		}, "1BAD", "name must match"},
		{"invalid punctuation", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"BAD-NAME": "x"}
		}, "BAD-NAME", "name must match"},
		{"empty name", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"": "x"}
		}, "", "name must match"},
		{"nul value", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"PROFILE": "bad\x00value"}
		}, "PROFILE", "value must be nonempty"},
		{"empty value", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"PROFILE": "  "}
		}, "PROFILE", "value must be nonempty"},
		{"case duplicate env", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"PROFILE": secret, "profile": secret}
		}, "profile", "duplicates"},
		{"case duplicate passthrough", func(v map[string]any) {
			projectConfig(v)["envPassthrough"] = []any{"TEAM_TOKEN", "team_token"}
		}, "team_token", "duplicates"},
		{"env and passthrough overlap", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"PROFILE": secret}
			projectConfig(v)["envPassthrough"] = []any{"profile"}
		}, "profile", "also configured literally"},
		{"derived app secret", func(v map[string]any) {
			projectConfig(v)["env"] = map[string]any{"FEISHU_APP_SECRET": secret}
		}, "FEISHU_APP_SECRET", "reserved"},
		{"derived token env case", func(v map[string]any) {
			projectConfig(v)["envPassthrough"] = []any{"model_auth_token"}
		}, "model_auth_token", "reserved"},
		{"derived header env", func(v map[string]any) {
			v["model"].(map[string]any)["extraHeaders"] = map[string]any{"X-Token": "env:TEAM_HEADER_SECRET"}
			projectConfig(v)["env"] = map[string]any{"team_header_secret": secret}
		}, "team_header_secret", "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			tc.mutate(v)
			_, err := parseFixture(t, v)
			if err == nil {
				t.Fatal("accepted unsafe project environment")
			}
			var envErr *ProjectEnvError
			if !errors.As(err, &envErr) {
				t.Fatalf("error is not typed: %v", err)
			}
			if envErr.Project != "assistant" || envErr.Variable != tc.variable || !strings.Contains(envErr.Reason, tc.reason) {
				t.Fatalf("unexpected error: %#v", envErr)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "bad\x00value") {
				t.Fatalf("error exposed a value: %v", err)
			}
		})
	}
}

func TestProjectEnvironmentRejectsReservedNames(t *testing.T) {
	reserved := []string{"HOME", "home", "UserProfile", "HOMEDRIVE", "homedrive", "HOMEPATH", "CLAUDE_CONFIG_DIR", "claude_config_dir", "CLAUDECODE", "claudecode", "NODE_OPTIONS", "node_options", "DEBUG", "debug", "ANTHROPIC_BASE_URL", "anthropic_api_key", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_AUTO_MODE_SERVER", "claude_code_x", "CLAUDE_AGENT_SDK", "claude_agent_sdk_v2", "FEISHU_ACTION_TOKEN", "feishu_action_client", "FEISHU_REQUEST_CHAT_ID", "feishu_request_sender_open_id"}
	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			for _, field := range []string{"env", "envPassthrough"} {
				v := fixture(t)
				p := projectConfig(v)
				if field == "env" {
					p["env"] = map[string]any{name: "value"}
				} else {
					p["envPassthrough"] = []any{name}
				}
				_, err := parseFixture(t, v)
				var envErr *ProjectEnvError
				if !errors.As(err, &envErr) || envErr.Project != "assistant" || envErr.Variable != name || !strings.Contains(envErr.Reason, "reserved") {
					t.Fatalf("%s accepted %q: %v", field, name, err)
				}
			}
		})
	}
}

func TestProjectEnvironmentRejectsFixedActionCredentialSources(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	action := func() map[string]any {
		return map[string]any{
			"id": "publish", "description": "Publish status", "executable": exe,
			"env": map[string]any{"TARGET": "ACTION_SOURCE_SECRET"}, "parameters": map[string]any{},
		}
	}
	for _, field := range []string{"env", "envPassthrough"} {
		for _, name := range []string{"ACTION_SOURCE_SECRET", "action_source_secret"} {
			v := fixture(t)
			p := projectConfig(v)
			p["actions"] = []any{action()}
			if field == "env" {
				p["env"] = map[string]any{name: "value"}
			} else {
				p["envPassthrough"] = []any{name}
			}
			_, err := parseFixture(t, v)
			var envErr *ProjectEnvError
			if !errors.As(err, &envErr) || envErr.Variable != name || !strings.Contains(envErr.Reason, "fixed action assistant/publish") {
				t.Fatalf("%s accepted action credential source %q: %v", field, name, err)
			}
		}
	}

	// A target name is not a credential source and remains available.
	v := fixture(t)
	p := projectConfig(v)
	p["actions"] = []any{action()}
	p["env"] = map[string]any{"TARGET": "literal"}
	if _, err := parseFixture(t, v); err != nil {
		t.Fatalf("action target name was treated as a source: %v", err)
	}

	// Action sources are reserved for every project, not only their owner.
	v = fixture(t)
	v["projects"] = append(v["projects"].([]any), map[string]any{
		"id": "other", "maintainers": []any{"ou_example_other"}, "actions": []any{action()},
	})
	projectConfig(v)["envPassthrough"] = []any{"ACTION_SOURCE_SECRET"}
	_, err = parseFixture(t, v)
	var envErr *ProjectEnvError
	if !errors.As(err, &envErr) || envErr.Project != "assistant" || !strings.Contains(envErr.Reason, "fixed action other/publish") {
		t.Fatalf("cross-project action source was not reserved: %v", err)
	}
}

func TestActionEnvironmentRejectsRequestContextKeys(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"FEISHU_REQUEST_CHAT_ID", "feishu_request_sender_name", "FEISHU_ACTION_REQUEST_ID", "feishu_action_approver_open_id"} {
		want := "FEISHU_REQUEST_"
		if strings.HasPrefix(strings.ToUpper(key), "FEISHU_ACTION_") {
			want = "FEISHU_ACTION_"
		}
		for _, field := range []string{"env", "vars"} {
			v := fixture(t)
			p := projectConfig(v)
			p["actions"] = []any{map[string]any{"id": "publish", "description": "Publish", "executable": exe, "parameters": map[string]any{}}}
			p["actions"].([]any)[0].(map[string]any)[field] = map[string]any{key: "value"}
			data, _ := json.Marshal(v)
			_, err := Parse(data, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s accepted %q: %v", field, key, err)
			}
		}
	}
}

func TestProjectEnvironmentRejectsNullContainers(t *testing.T) {
	for _, field := range []string{"env", "envPassthrough"} {
		v := fixture(t)
		projectConfig(v)[field] = nil
		if _, err := parseFixture(t, v); err == nil {
			t.Fatalf("accepted null %s", field)
		}
	}
}

func TestSecretsIncludeLongPassthroughValues(t *testing.T) {
	t.Setenv("TEAM_API_TOKEN", "long-secret-value")
	t.Setenv("PROFILE", "team")
	v := fixture(t)
	p := projectConfig(v)
	p["envPassthrough"] = []any{"TEAM_API_TOKEN", "PROFILE"}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets := strings.Join(Secrets(c), "\n")
	if !strings.Contains(secrets, "long-secret-value") {
		t.Fatal("long passthrough value was not redacted")
	}
	if strings.Contains(secrets, "team") {
		t.Fatalf("short passthrough value was used as a redaction marker: %q", secrets)
	}
	if os.Getenv("TEAM_API_TOKEN") != "long-secret-value" {
		t.Fatal("test environment changed")
	}
}
