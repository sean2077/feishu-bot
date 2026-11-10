package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func projectSettingsConfig(p core.Project) core.Config {
	return core.Config{
		AppSecretEnv: "DEPLOY_APP_SECRET",
		Model:        core.ModelConfig{TokenEnv: "DEPLOY_MODEL_TOKEN"},
		Projects:     []core.Project{p},
	}
}

func writeProjectSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadProjectSettings(t *testing.T, path string, p core.Project) (ProjectSettings, error) {
	t.Helper()
	p.SettingsFile = path
	return LoadProjectSettings(projectSettingsConfig(p), p)
}

func TestLoadProjectSettingsAcceptsAdministratorShape(t *testing.T) {
	body := `{
  "env": {"EXAMPLE_PROFILE": "team"},
  "permissions": {
    "allow": ["Bash(uv run --no-sync example-tool sub:*)"],
    "ask": ["Bash(uv run --no-sync example-tool doc *--input*)"],
    "deny": ["Bash(curl:*)", "Read(./.secrets/**)"]
  },
  "autoMode": {
    "environment": "classifier guidance",
    "allow": ["Bash(echo:*)"],
    "soft_deny": ["git push"],
    "futureField": {"nested": true}
  }
}`
	path := writeProjectSettings(t, body)
	settings, err := loadProjectSettings(t, path, core.Project{ID: "team-tools"})
	if err != nil {
		t.Fatal(err)
	}
	var autoMode map[string]any
	if err := json.Unmarshal(settings.AutoMode, &autoMode); err != nil {
		t.Fatal(err)
	}
	if autoMode["environment"] != "classifier guidance" || autoMode["futureField"] == nil {
		t.Fatalf("autoMode not passed through: %#v", autoMode)
	}
	if settings.Env["EXAMPLE_PROFILE"] != "team" {
		t.Fatalf("env not preserved: %#v", settings.Env)
	}
	if got := settings.Permissions["allow"]; len(got) != 1 || got[0] != "Bash(uv run --no-sync example-tool sub:*)" {
		t.Fatalf("allow rules = %#v", got)
	}
	if got := settings.Permissions["ask"]; len(got) != 1 || got[0] != "Bash(uv run --no-sync example-tool doc *--input*)" {
		t.Fatalf("ask rules = %#v", got)
	}
	if got := settings.Permissions["deny"]; len(got) != 2 || got[0] != "Bash(curl:*)" || got[1] != "Read(./.secrets/**)" {
		t.Fatalf("deny rules = %#v", got)
	}
}

func TestLoadProjectSettingsRejectsInvalidContent(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		field        string
		reason       string
		projectSetup func(*core.Project)
		configSetup  func(*core.Config)
	}{
		{name: "unknown top-level", body: `{"hooks":{}}`, field: "hooks", reason: "unknown field"},
		{name: "unknown defaultMode", body: `{"defaultMode":"acceptEdits"}`, field: "defaultMode", reason: "unknown field"},
		{name: "unknown apiKeyHelper", body: `{"apiKeyHelper":"/tmp/helper"}`, field: "apiKeyHelper", reason: "unknown field"},
		{name: "autoMode array", body: `{"autoMode":[]}`, field: "autoMode", reason: "must be an object"},
		{name: "autoMode string", body: `{"autoMode":"x"}`, field: "autoMode", reason: "must be an object"},
		{name: "autoMode null", body: `{"autoMode":null}`, field: "autoMode", reason: "must be an object"},
		{name: "unknown permission", body: `{"permissions":{"defaultMode":"acceptEdits"}}`, field: "permissions.defaultMode", reason: "unknown field"},
		{name: "env array", body: `{"env":[]}`, field: "env", reason: "must be an object"},
		{name: "env null", body: `{"env":null}`, field: "env", reason: "must be an object"},
		{name: "env value number", body: `{"env":{"EXAMPLE_PROFILE":1}}`, field: "env.EXAMPLE_PROFILE", reason: "must be a string"},
		{name: "env value empty", body: `{"env":{"EXAMPLE_PROFILE":"  "}}`, field: "env.EXAMPLE_PROFILE", reason: "nonempty"},
		{name: "env value nul", body: `{"env":{"EXAMPLE_PROFILE":"\u0000"}}`, field: "env.EXAMPLE_PROFILE", reason: "nonempty"},
		{name: "env invalid name", body: `{"env":{"1BAD":"x"}}`, field: "env.1BAD", reason: "name must match"},
		{name: "env reserved", body: `{"env":{"HOME":"value-marker"}}`, field: "env.HOME", reason: "reserved"},
		{name: "env auto mode reserved", body: `{"env":{"CLAUDE_CODE_AUTO_MODE_SERVER":"1"}}`, field: "env.CLAUDE_CODE_AUTO_MODE_SERVER", reason: "reserved"},
		{name: "env app secret", body: `{"env":{"DEPLOY_APP_SECRET":"value-marker"}}`, field: "env.DEPLOY_APP_SECRET", reason: "reserved"},
		{name: "env model token", body: `{"env":{"DEPLOY_MODEL_TOKEN":"value-marker"}}`, field: "env.DEPLOY_MODEL_TOKEN", reason: "reserved"},
		{name: "env request context", body: `{"env":{"FEISHU_REQUEST_CHAT_ID":"value-marker"}}`, field: "env.FEISHU_REQUEST_CHAT_ID", reason: "reserved"},
		{name: "env header", body: `{"env":{"TEAM_HEADER_SECRET":"value-marker"}}`, field: "env.TEAM_HEADER_SECRET", reason: "reserved", configSetup: func(c *core.Config) {
			c.Model.ExtraHeaders = map[string]string{"X-Team": "env:TEAM_HEADER_SECRET"}
		}},
		{name: "env action source", body: `{"env":{"ACTION_SOURCE_SECRET":"value-marker"}}`, field: "env.ACTION_SOURCE_SECRET", reason: "fixed action", projectSetup: func(p *core.Project) {
			p.Actions = []core.ActionSpec{{ID: "publish", Env: map[string]string{"TARGET": "ACTION_SOURCE_SECRET"}}}
		}},
		{name: "env duplicate case", body: `{"env":{"EXAMPLE_PROFILE":"a","example_profile":"b"}}`, field: "env.example_profile", reason: "duplicates"},
		{name: "env project literal overlap", body: `{"env":{"example_profile":"a"}}`, field: "env.example_profile", reason: "projects[].env", projectSetup: func(p *core.Project) {
			p.Env = map[string]string{"EXAMPLE_PROFILE": "literal-value"}
		}},
		{name: "env project passthrough overlap", body: `{"env":{"example_profile":"a"}}`, field: "env.example_profile", reason: "envPassthrough", projectSetup: func(p *core.Project) {
			p.EnvPassthrough = []string{"EXAMPLE_PROFILE"}
		}},
		{name: "permissions array", body: `{"permissions":[]}`, field: "permissions", reason: "must be an object"},
		{name: "permissions null", body: `{"permissions":null}`, field: "permissions", reason: "must be an object"},
		{name: "allow null", body: `{"permissions":{"allow":null}}`, field: "permissions.allow", reason: "must be an array"},
		{name: "allow object", body: `{"permissions":{"allow":{}}}`, field: "permissions.allow", reason: "must be an array"},
		{name: "allow non-string", body: `{"permissions":{"allow":["rule-marker",1]}}`, field: "permissions.allow[1]", reason: "must be a string"},
		{name: "allow empty", body: `{"permissions":{"allow":[""]}}`, field: "permissions.allow[0]", reason: "nonempty"},
		{name: "allow nul", body: `{"permissions":{"allow":["\u0000"]}}`, field: "permissions.allow[0]", reason: "nonempty"},
		{name: "deny invalid", body: `{"permissions":{"deny":[""]}}`, field: "permissions.deny[0]", reason: "nonempty"},
		{name: "malformed", body: `{"env":`, field: "file", reason: "must be a JSON object"},
		{name: "trailing", body: `{} {}`, field: "file", reason: "must be a JSON object"},
		{name: "top array", body: `[]`, field: "file", reason: "must be a JSON object"},
		{name: "top string", body: `"x"`, field: "file", reason: "must be a JSON object"},
		{name: "top null", body: `null`, field: "file", reason: "must be a JSON object"},
		{name: "empty file", body: ``, field: "file", reason: "must be a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := core.Project{ID: "team-tools"}
			if tc.projectSetup != nil {
				tc.projectSetup(&p)
			}
			c := projectSettingsConfig(p)
			if tc.configSetup != nil {
				tc.configSetup(&c)
			}
			path := writeProjectSettings(t, tc.body)
			p.SettingsFile = path
			c.Projects = []core.Project{p}
			_, err := LoadProjectSettings(c, p)
			assertProjectSettingsError(t, err, path, tc.field, tc.reason)
		})
	}
}

func assertProjectSettingsError(t *testing.T, err error, path, field, reason string) {
	t.Helper()
	var settingsErr *ProjectSettingsError
	if !errors.As(err, &settingsErr) {
		t.Fatalf("error is not typed: %v", err)
	}
	if settingsErr.Project != "team-tools" || settingsErr.Path != path || settingsErr.Field != field || !strings.Contains(settingsErr.Reason, reason) {
		t.Fatalf("unexpected error: %#v", settingsErr)
	}
	for _, marker := range []string{"value-marker", "rule-marker", "literal-value"} {
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error exposed content %q: %v", marker, err)
		}
	}
}

func TestLoadProjectSettingsRejectsBadFiles(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := loadProjectSettings(t, missing, core.Project{ID: "team-tools"}); err == nil {
		t.Fatal("accepted missing settings file")
	}
	if _, err := loadProjectSettings(t, t.TempDir(), core.Project{ID: "team-tools"}); err == nil {
		t.Fatal("accepted directory as settings file")
	}
	oversized := writeProjectSettings(t, strings.Repeat(" ", maxProjectSettingsBytes+1))
	if _, err := loadProjectSettings(t, oversized, core.Project{ID: "team-tools"}); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("accepted oversized settings file: %v", err)
	}
	invalidUTF8 := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(invalidUTF8, []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProjectSettings(t, invalidUTF8, core.Project{ID: "team-tools"}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("accepted invalid UTF-8: %v", err)
	}
}

func TestLoadProjectSettingsAllowsEmptySections(t *testing.T) {
	for _, body := range []string{`{}`, `{"env":{}}`, `{"permissions":{}}`, `{"permissions":{"allow":[]}}`} {
		path := writeProjectSettings(t, body)
		if _, err := loadProjectSettings(t, path, core.Project{ID: "team-tools"}); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
	}
}

func TestLoadProjectSettingsReadsCurrentSnapshot(t *testing.T) {
	path := writeProjectSettings(t, `{"env":{"EXAMPLE_PROFILE":"first"}}`)
	first, err := loadProjectSettings(t, path, core.Project{ID: "team-tools"})
	if err != nil || first.Env["EXAMPLE_PROFILE"] != "first" {
		t.Fatal(first, err)
	}
	if err := os.WriteFile(path, []byte(`{"env":{"EXAMPLE_PROFILE":"second"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := loadProjectSettings(t, path, core.Project{ID: "team-tools"})
	if err != nil || second.Env["EXAMPLE_PROFILE"] != "second" {
		t.Fatal(second, err)
	}
}

func TestProjectSettingsFilePathResolvesRelativeToConfig(t *testing.T) {
	v := fixture(t)
	p := projectConfig(v)
	p["settingsFile"] = "settings/team.json"
	p["cwd"] = "work"
	data, _ := json.Marshal(v)
	base := t.TempDir()
	c, err := Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "settings", "team.json"); c.Projects[0].SettingsFile != want {
		t.Fatalf("settingsFile = %q, want %q", c.Projects[0].SettingsFile, want)
	}
}

func TestProjectSettingsFileRejectsNullAndEmpty(t *testing.T) {
	for _, value := range []any{nil, ""} {
		v := fixture(t)
		projectConfig(v)["settingsFile"] = value
		data, _ := json.Marshal(v)
		if _, err := Parse(data, t.TempDir()); err == nil {
			t.Fatalf("accepted settingsFile %#v", value)
		}
	}
}
