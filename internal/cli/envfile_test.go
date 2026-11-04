package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func TestParseEnvFile(t *testing.T) {
	input := "\xEF\xBB\xBF# comment\r\nA=1\r\nB=two=three\r\nC=# literal\r\nA=last\r\n"
	values, err := parseEnvFile([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if values["A"] != "last" || values["B"] != "two=three" || values["C"] != "# literal" {
		t.Fatalf("unexpected values: %#v", values)
	}
	for _, bad := range []string{"NOVALUE", "BAD KEY=value", "A=bad\nvalue", "A=bad\x00value"} {
		if _, err := parseEnvFile([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid environment file %q", bad)
		}
	}
	if _, err := parseEnvFile(make([]byte, maxEnvFileBytes+1)); err == nil {
		t.Fatal("accepted oversized environment file")
	}
	_, err = parseEnvFile([]byte("BAD KEY=super-secret"))
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("invalid-key error leaked a value: %v", err)
	}
}

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.env")
	if err := os.WriteFile(path, []byte("CLI_ENV_TEST=value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("CLI_ENV_TEST") })
	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CLI_ENV_TEST") != "value" {
		t.Fatal("environment variable was not applied")
	}
}

func TestCredentialNamesUsesActionSource(t *testing.T) {
	c := config.Defaults()
	c.AppSecretEnv = "APP_SECRET"
	c.Model.TokenEnv = "MODEL_TOKEN"
	c.Model.ExtraHeaders = map[string]string{"X-Test": "env:HEADER_SECRET"}
	c.Projects = []core.Project{{
		ID: "p",
		Actions: []core.ActionSpec{{
			ID:  "action",
			Env: map[string]string{"TARGET": "SOURCE_SECRET"},
		}},
	}}
	names := strings.Join(credentialNames(c), ",")
	for _, want := range []string{"APP_SECRET", "MODEL_TOKEN", "SOURCE_SECRET", "HEADER_SECRET"} {
		if !strings.Contains(names, want) {
			t.Fatalf("credential names %q missing %q", names, want)
		}
	}
	if strings.Contains(names, "TARGET") {
		t.Fatalf("target environment name was treated as a source: %q", names)
	}
}

func TestBuildEnvFileUsesCredentialsAndAllowlist(t *testing.T) {
	t.Setenv("APP_SECRET", "app-value")
	t.Setenv("MODEL_TOKEN", "model-value")
	t.Setenv("PATH", "/tmp/test-bin")
	t.Setenv("TZ", "UTC")
	c := config.Defaults()
	c.AppSecretEnv = "APP_SECRET"
	c.Model.TokenEnv = "MODEL_TOKEN"
	data, missing, err := buildEnvFile(c)
	if err != nil || len(missing) != 0 {
		t.Fatal(err, missing)
	}
	values, err := parseEnvFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if values["APP_SECRET"] != "app-value" || values["MODEL_TOKEN"] != "model-value" {
		t.Fatalf("credentials missing: %#v", values)
	}
	if values["PATH"] != "/tmp/test-bin" || values["TZ"] != "UTC" {
		t.Fatalf("allowlisted runtime values missing: %#v", values)
	}
	if _, ok := values["HOME"]; ok {
		t.Fatal("HOME must not be captured into the service environment")
	}
}

func TestMissingCredentials(t *testing.T) {
	c := config.Defaults()
	c.AppSecretEnv = "MISSING_SECRET"
	missing := missingCredentials(c, map[string]string{"MODEL_AUTH_TOKEN": "value"})
	if len(missing) != 1 || missing[0] != "MISSING_SECRET" {
		t.Fatalf("unexpected missing credentials: %#v", missing)
	}
	if got := missingCredentials(c, map[string]string{"MISSING_SECRET": "value", "MODEL_AUTH_TOKEN": "value"}); len(got) != 0 {
		t.Fatalf("unexpected missing credentials: %#v", got)
	}
}
