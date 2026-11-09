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

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	previous, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func TestProjectEnvChecksReportPresenceAndSuspiciousLiterals(t *testing.T) {
	t.Setenv("SET_PASSTHROUGH", "present-value")
	const missing = "MISSING_PASSTHROUGH_FOR_TEST"
	unsetEnv(t, missing)
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.Projects = []core.Project{{
		ID:             "team",
		Env:            map[string]string{"PROFILE": "team", "TEAM_TOKEN": "literal-secret"},
		EnvPassthrough: []string{missing, "SET_PASSTHROUGH"},
	}}
	checks := projectEnvChecks(c)
	byName := map[string]Diagnostic{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if check, ok := byName["project team credential SET_PASSTHROUGH"]; !ok || !check.OK {
		t.Fatalf("present passthrough not reported: %#v", checks)
	}
	if check, ok := byName["project team credential "+missing]; !ok || check.OK {
		t.Fatalf("missing passthrough not reported as failed: %#v", checks)
	}
	if check, ok := byName["project team env TEAM_TOKEN"]; !ok || check.OK {
		t.Fatalf("credential-looking literal not reported: %#v", checks)
	}
	if _, ok := byName["project team env PROFILE"]; ok {
		t.Fatalf("ordinary literal was flagged: %#v", checks)
	}
	for _, check := range checks {
		if strings.Contains(check.Detail, "present-value") || strings.Contains(check.Detail, "literal-secret") {
			t.Fatalf("diagnostic exposed a value: %#v", check)
		}
	}
	for _, check := range deploymentChecks(c) {
		if strings.HasPrefix(check.Name, "project team ") {
			t.Fatalf("project environment blocked foreground run: %#v", check)
		}
	}
	report := RunDoctor(context.Background(), c, false, false)
	found := false
	for _, check := range report.Checks {
		found = found || check.Name == "project team credential "+missing
	}
	if !found {
		t.Fatal("doctor omitted project environment checks")
	}
	if remedy := diagnosticRemedy("project team credential " + missing); !strings.Contains(remedy, "--env-file") {
		t.Fatalf("missing passthrough remedy lacks the service.env update path: %s", remedy)
	}
	if remedy := diagnosticRemedy("project team env TEAM_TOKEN"); !strings.Contains(remedy, "envPassthrough") {
		t.Fatalf("literal remedy does not point to passthrough: %s", remedy)
	}
}

func TestProjectEnvChecksTreatPlaceholderAsMissing(t *testing.T) {
	t.Setenv("PLACEHOLDER_PASSTHROUGH", "your-token-here")
	c := config.Defaults()
	c.Projects = []core.Project{{ID: "team", EnvPassthrough: []string{"PLACEHOLDER_PASSTHROUGH"}}}
	for _, check := range projectEnvChecks(c) {
		if check.Name == "project team credential PLACEHOLDER_PASSTHROUGH" && check.OK {
			t.Fatal("placeholder passthrough value was accepted")
		}
	}
}

func explainConfigFile(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
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
	mutate(value)
	data, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigExplainReportsProjectEnvironmentNamesOnly(t *testing.T) {
	t.Setenv("SET_PASSTHROUGH", "present-value")
	const missing = "MISSING_PASSTHROUGH_FOR_TEST"
	unsetEnv(t, missing)
	path := explainConfigFile(t, func(v map[string]any) {
		p := v["projects"].([]any)[0].(map[string]any)
		p["env"] = map[string]any{"PROFILE": "team", "TEAM_REGION": "private-region"}
		p["envPassthrough"] = []string{"SET_PASSTHROUGH", missing}
	})
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
	if got := strings.Join(anyStrings(view["projectEnv"]), ","); got != "PROFILE,TEAM_REGION" {
		t.Fatalf("project env names = %q", got)
	}
	if got := strings.Join(anyStrings(view["projectEnvPassthrough"]), ","); got != missing+",SET_PASSTHROUGH" {
		t.Fatalf("passthrough names = %q", got)
	}
	if got := strings.Join(anyStrings(view["projectEnvPassthroughMissing"]), ","); got != missing {
		t.Fatalf("missing passthrough names = %q", got)
	}
	for _, value := range []string{"team", "private-region", "present-value"} {
		if strings.Contains(out.String(), value) {
			t.Fatalf("config explain exposed a project environment value %q: %s", value, out.String())
		}
	}
	o.JSON = false
	out.Reset()
	if err := runConfigExplain(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Project env names: PROFILE, TEAM_REGION") || !strings.Contains(out.String(), "Missing passthrough: "+missing) {
		t.Fatalf("text explain omitted environment names: %s", out.String())
	}
}

func anyStrings(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func TestDoctorLoadFailureShowsProjectEnvNameOnly(t *testing.T) {
	path := explainConfigFile(t, func(v map[string]any) {
		v["projects"].([]any)[0].(map[string]any)["env"] = map[string]any{"HOME": "home-value"}
	})
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--config", path}, &stdout, &stderr); code == 0 {
		t.Fatal("doctor accepted a reserved project variable")
	}
	if !strings.Contains(stderr.String(), "project p") || !strings.Contains(stderr.String(), "HOME") {
		t.Fatalf("doctor did not identify the project variable: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "home-value") {
		t.Fatalf("doctor exposed the project variable value: %s", stderr.String())
	}
}

func TestConfigExplainSurfacesProjectEnvValidationOnly(t *testing.T) {
	path := explainConfigFile(t, func(v map[string]any) {
		v["projects"].([]any)[0].(map[string]any)["env"] = map[string]any{"HOME": "home-value"}
	})
	o, err := Parse([]string{"config", "explain", "--config", path, "--chat", "chat", "--user", "user", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	err = runConfigExplain(context.Background(), o, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "project p") || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("reserved-name error was not surfaced: %v", err)
	}
	if strings.Contains(err.Error(), "home-value") {
		t.Fatalf("reserved-name error exposed a value: %v", err)
	}
	path = explainConfigFile(t, func(v map[string]any) {
		v["projects"].([]any)[0].(map[string]any)["maintainers"] = []string{"*"}
	})
	o, _ = Parse([]string{"config", "explain", "--config", path, "--chat", "chat", "--user", "user", "--json"})
	err = runConfigExplain(context.Background(), o, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cannot be loaded or validated") {
		t.Fatalf("unrelated validation error was not kept generic: %v", err)
	}
}

func TestBuildEnvFileCapturesOptionalProjectPassthrough(t *testing.T) {
	t.Setenv("APP_SECRET", "app-value")
	t.Setenv("MODEL_TOKEN", "model-value")
	t.Setenv("TEAM_API_TOKEN", "token-value")
	const missing = "MISSING_PASSTHROUGH_FOR_TEST"
	unsetEnv(t, missing)
	c := config.Defaults()
	c.AppSecretEnv = "APP_SECRET"
	c.Model.TokenEnv = "MODEL_TOKEN"
	c.Projects = []core.Project{{ID: "p", EnvPassthrough: []string{"TEAM_API_TOKEN", missing}}}
	data, missingNames, err := buildEnvFile(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(missingNames) != 0 {
		t.Fatalf("optional passthrough was treated as a required credential: %#v", missingNames)
	}
	values, err := parseEnvFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if values["TEAM_API_TOKEN"] != "token-value" {
		t.Fatalf("passthrough value was not captured: %#v", values)
	}
	if _, ok := values[missing]; ok {
		t.Fatal("missing passthrough was written with an empty value")
	}
	if got := missingCredentials(c, map[string]string{"APP_SECRET": "a", "MODEL_TOKEN": "m"}); len(got) != 0 {
		t.Fatalf("missing passthrough blocked credential validation: %#v", got)
	}
}

func TestInstallRefusesMissingProjectPassthroughBeforeChangingInstallation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FEISHU_BOT_HOME", filepath.Join(dir, "home"))
	const passthrough = "NEW_PASSTHROUGH_TOKEN_FOR_TEST"
	unsetEnv(t, passthrough)
	configPath := explainConfigFile(t, func(v map[string]any) {
		v["projects"].([]any)[0].(map[string]any)["envPassthrough"] = []string{passthrough}
	})
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	existing := "TEST_APP_SECRET=old-app\nTEST_MODEL_TOKEN=old-model\n"
	envPath := filepath.Join(work, "service.env")
	if err := os.WriteFile(envPath, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeServiceManager{platform: "test", present: true, owned: true}
	o := Options{Command: "daemon", Subcommand: "install", ConfigPath: configPath, ConfigExplicit: true, WorkDir: work, NoStart: true}
	err := installWithManager(context.Background(), o, manager, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), passthrough) {
		t.Fatalf("installation did not report the missing passthrough: %v", err)
	}
	if data, _ := os.ReadFile(envPath); string(data) != existing {
		t.Fatalf("failed installation changed service.env: %q", data)
	}
	if manager.installed || manager.started || manager.restored {
		t.Fatalf("failed installation changed the service definition: %#v", manager)
	}
	if _, err := os.Stat(filepath.Join(work, "bin", executableName())); !os.IsNotExist(err) {
		t.Fatal("failed installation published a binary")
	}
}
