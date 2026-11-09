package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

const cliSettingsBody = `{
  "env": {"EXAMPLE_PROFILE": "team"},
  "permissions": {
    "allow": ["Bash(uv run --no-sync example-tool sub:*)"],
    "ask": ["Bash(uv run --no-sync example-tool doc *--input*)"],
    "deny": ["Bash(curl:*)", "Read(./.secrets/**)"]
  }
}`

func writeCLISettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectSettingsChecksReportSchemaWithoutRules(t *testing.T) {
	valid := writeCLISettings(t, cliSettingsBody)
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.Projects = []core.Project{{ID: "team-tools", SettingsFile: valid}}
	checks := projectSettingsChecks(c)
	if len(checks) != 1 || !checks[0].OK || checks[0].Name != "project team-tools settings file" {
		t.Fatalf("valid settings checks: %#v", checks)
	}
	for _, marker := range []string{"Bash(uv run", "Read(./.secrets", "team"} {
		if strings.Contains(checks[0].Detail, marker) {
			t.Fatalf("doctor exposed settings content %q: %s", marker, checks[0].Detail)
		}
	}
	invalid := writeCLISettings(t, `{"env":{"HOME":"value-marker"},"permissions":{"allow":["rule-marker",1]}}`)
	c.Projects[0].SettingsFile = invalid
	checks = projectSettingsChecks(c)
	if len(checks) != 1 || checks[0].OK {
		t.Fatalf("invalid settings checks: %#v", checks)
	}
	if !strings.Contains(checks[0].Detail, "env.HOME") {
		t.Fatalf("doctor did not report the field: %s", checks[0].Detail)
	}
	for _, marker := range []string{"value-marker", "rule-marker"} {
		if strings.Contains(checks[0].Detail, marker) {
			t.Fatalf("doctor exposed settings content %q: %s", marker, checks[0].Detail)
		}
	}
	for _, check := range deploymentChecks(c) {
		if strings.Contains(check.Name, "settings file") {
			t.Fatalf("settings file blocked foreground run: %#v", check)
		}
	}
	report := RunDoctor(context.Background(), c, false, false)
	found := false
	for _, check := range report.Checks {
		if check.Name == "project team-tools settings file" {
			found = true
			if check.OK {
				t.Fatal("doctor accepted an invalid settings file")
			}
			if strings.Contains(check.Detail, "value-marker") || strings.Contains(check.Detail, "rule-marker") {
				t.Fatalf("doctor report exposed settings content: %s", check.Detail)
			}
		}
	}
	if !found {
		t.Fatal("doctor omitted the project settings check")
	}
}

func TestConfigExplainShowsSettingsFileWithoutReadingIt(t *testing.T) {
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
	missing := filepath.Join(t.TempDir(), "not-read.json")
	value["projects"].([]any)[0].(map[string]any)["settingsFile"] = missing
	data, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	o, err := Parse([]string{"config", "explain", "--config", path, "--chat", "private-chat", "--user", "ou_example", "--chat-type", "p2p", "--json"})
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
	if view["settingsFile"] != missing {
		t.Fatalf("settingsFile = %#v, want %q", view["settingsFile"], missing)
	}
	if strings.Contains(out.String(), "uv run --no-sync") || strings.Contains(out.String(), "curl:*") {
		t.Fatalf("config explain read settings content: %s", out.String())
	}
	o.JSON = false
	out.Reset()
	if err := runConfigExplain(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Settings file: "+missing) {
		t.Fatalf("text explain omitted the settings path: %s", out.String())
	}
}
