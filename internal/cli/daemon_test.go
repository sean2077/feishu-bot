package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func TestParseDaemonOptions(t *testing.T) {
	o, err := Parse([]string{"daemon", "install", "--config", "config.json", "--env-file", "service.env", "--work-dir", "work", "--log-file", "service.log", "--log-max-size", "20", "--log-max-backups", "2", "--binary", "feishu-bot.exe", "--replace", "--no-start"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Command != "daemon" || o.Subcommand != "install" {
		t.Fatalf("unexpected command: %#v", o)
	}
	if !o.Replace || !o.NoStart || o.LogSize != 20 || o.LogKeep != 2 {
		t.Fatalf("unexpected flags: %#v", o)
	}
	for name, value := range map[string]string{"config": o.ConfigPath, "env": o.EnvFile, "work": o.WorkDir, "log": o.LogFile, "binary": o.Binary} {
		if !filepath.IsAbs(value) {
			t.Fatalf("%s path is not absolute: %q", name, value)
		}
	}
	o, err = Parse([]string{"daemon", "logs", "-f", "-n", "50", "--log-file", "service.log"})
	if err != nil || !o.Follow || o.LogLines != 50 || !filepath.IsAbs(o.LogFile) {
		t.Fatal(o, err)
	}
	if o, err = Parse([]string{"daemon", "status", "--json"}); err != nil || !o.JSON {
		t.Fatal(o, err)
	}
	if o, err = Parse([]string{"daemon", "start", "--reclaim-stale-lock"}); err != nil || !o.Reclaim {
		t.Fatal(o, err)
	}
	if o, err = Parse([]string{"config", "fmt", "--config", "config.json"}); err != nil || o.Subcommand != "fmt" || !o.ConfigExplicit {
		t.Fatal(o, err)
	}
}

func TestParseDaemonHelp(t *testing.T) {
	for _, args := range [][]string{{"daemon", "--help"}, {"daemon", "install", "--help"}, {"config", "--help"}, {"config", "path", "--help"}} {
		o, err := Parse(args)
		if err != nil || o.Command != "help" {
			t.Fatalf("%q: %#v %v", args, o, err)
		}
	}
}

func TestParseDaemonRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{
		{"daemon", "bogus"},
		{"config", "bogus"},
		{"daemon", "install", "--probe"},
		{"daemon", "status", "-n", "5"},
		{"daemon", "logs", "--json"},
		{"daemon", "install", "--log-max-size", "0"},
		{"daemon", "install", "--config"},
		{"daemon", "start", "--reclaim-stale-lock", "--reclaim-stale-lock"},
		{"config", "example", "--config", "x"},
		{"run", "--service"},
		{"run", "--log-max-size", "10"},
		{"run", "--log-max-backups", "2"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	o, err := Parse([]string{"run", "--service", "--log-file", "service.log"})
	if err != nil || !o.Service || o.LogFile == "" {
		t.Fatal(o, err)
	}
}

func TestDaemonMetaRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FEISHU_BOT_HOME", home)
	meta := daemonMeta{
		Schema: 1, Version: "test", Name: "feishu-bot", Platform: "test",
		WorkDir: "/work", BinaryPath: "/work/bin/feishu-bot", ConfigPath: "/work/config.json",
		EnvFile: "/work/service.env", DataDir: "/work/state", LogFile: "/work/logs/service.log",
		LogMaxSize: 10 << 20, LogMaxBackups: 3, InstalledAt: "2026-11-04T00:00:00Z",
	}
	if err := saveDaemonMeta(meta); err != nil {
		t.Fatal(err)
	}
	got, err := loadDaemonMeta()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, meta) {
		t.Fatalf("metadata changed: %#v", got)
	}
	path, err := daemonMetaPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDaemonMeta(); err == nil {
		t.Fatal("accepted corrupt daemon metadata")
	}
}

type fakeServiceManager struct {
	platform   string
	present    bool
	owned      bool
	checkErr   error
	installErr error
	startErr   error
	installed  bool
	started    bool
	spec       serviceSpec
}

func (f *fakeServiceManager) Platform() string { return f.platform }
func (f *fakeServiceManager) Check(_ context.Context, _ serviceSpec, replace bool) error {
	if f.checkErr != nil {
		return f.checkErr
	}
	if f.present && !f.owned && !replace {
		return errors.New("foreign service definition")
	}
	return nil
}
func (f *fakeServiceManager) Install(_ context.Context, spec serviceSpec, _ bool) error {
	f.installed = true
	f.spec = spec
	return f.installErr
}
func (f *fakeServiceManager) Uninstall(context.Context, string) error { return nil }
func (f *fakeServiceManager) Start(context.Context, string) error {
	f.started = true
	return f.startErr
}
func (f *fakeServiceManager) Present(context.Context, string) (bool, error) {
	return f.present, nil
}

func writeInstallConfig(t *testing.T, dir string) string {
	t.Helper()
	c := config.Defaults()
	c.TenantID = "tenant_local"
	c.AppID = "cli_local"
	c.AppSecretEnv = "TEST_APP_SECRET"
	c.Model.TokenEnv = "TEST_MODEL_TOKEN"
	c.DataDir = filepath.Join(dir, "state")
	c.Model.BaseURL = "https://model.example"
	c.Model.Model = "model"
	c.Model.SmallModel = "model"
	c.Projects = []core.Project{{ID: "p", Maintainers: []string{"ou_example_owner"}, PermissionMode: "acceptEdits", NativeConfig: "isolated", AllowedTools: []string{}, Actions: []core.ActionSpec{}}}
	c.Groups = []core.Group{}
	c.DMs = []core.DM{{UserID: "ou_example", ProjectID: "p"}}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstallWithManager(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FEISHU_BOT_HOME", filepath.Join(dir, "home"))
	configPath := writeInstallConfig(t, dir)
	envPath := filepath.Join(dir, "provided.env")
	if err := os.WriteFile(envPath, []byte("TEST_APP_SECRET=app-value\nTEST_MODEL_TOKEN=model-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceBinary := filepath.Join(dir, "source-binary")
	if err := os.WriteFile(sourceBinary, []byte("binary-content"), 0600); err != nil {
		t.Fatal(err)
	}
	oldPrecheck := installPrecheck
	installPrecheck = func(context.Context, core.Config) error { return nil }
	defer func() { installPrecheck = oldPrecheck }()
	manager := &fakeServiceManager{platform: "test"}
	o := Options{
		Command: "daemon", Subcommand: "install",
		ConfigPath: configPath, ConfigExplicit: true,
		EnvFile: envPath, WorkDir: filepath.Join(dir, "work"),
		LogFile: filepath.Join(dir, "work", "logs", "custom.log"),
		LogSize: 20, LogKeep: 2, NoStart: true, Binary: sourceBinary,
	}
	var stdout, stderr bytes.Buffer
	if err := installWithManager(context.Background(), o, manager, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !manager.installed || manager.started {
		t.Fatalf("unexpected manager state: %#v", manager)
	}
	if manager.spec.Binary != filepath.Join(o.WorkDir, "bin", executableName()) {
		t.Fatalf("binary path = %q", manager.spec.Binary)
	}
	if manager.spec.Config != configPath || manager.spec.LogFile != o.LogFile || manager.spec.LogMaxSize != 20 || manager.spec.LogMaxBackups != 2 {
		t.Fatalf("unexpected service spec: %#v", manager.spec)
	}
	copied, err := os.ReadFile(manager.spec.Binary)
	if err != nil || string(copied) != "binary-content" {
		t.Fatal(err, string(copied))
	}
	serviceEnv, err := os.ReadFile(manager.spec.EnvFile)
	if err != nil || !strings.Contains(string(serviceEnv), "TEST_MODEL_TOKEN=model-value") {
		t.Fatal(err, string(serviceEnv))
	}
	meta, err := loadDaemonMeta()
	if err != nil {
		t.Fatal(err)
	}
	if meta.ConfigPath != configPath || meta.DataDir != filepath.Join(dir, "state") || meta.WorkDir != o.WorkDir {
		t.Fatalf("unexpected metadata: %#v", meta)
	}
	if !strings.Contains(stdout.String(), "Installed feishu-bot") {
		t.Fatalf("unexpected output: %s", stdout.String())
	}
}

func TestInstallRefusesForeignService(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FEISHU_BOT_HOME", filepath.Join(dir, "home"))
	configPath := writeInstallConfig(t, dir)
	envPath := filepath.Join(dir, "provided.env")
	if err := os.WriteFile(envPath, []byte("TEST_APP_SECRET=app-value\nTEST_MODEL_TOKEN=model-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldPrecheck := installPrecheck
	installPrecheck = func(context.Context, core.Config) error { return nil }
	defer func() { installPrecheck = oldPrecheck }()
	manager := &fakeServiceManager{platform: "test", present: true, owned: false}
	o := Options{Command: "daemon", Subcommand: "install", ConfigPath: configPath, ConfigExplicit: true, EnvFile: envPath, WorkDir: filepath.Join(dir, "work"), NoStart: true}
	var stdout, stderr bytes.Buffer
	if err := installWithManager(context.Background(), o, manager, &stdout, &stderr); err == nil {
		t.Fatal("installed over a foreign service definition")
	}
	if manager.installed {
		t.Fatal("manager Install was called after ownership rejection")
	}
	if _, err := os.Stat(filepath.Join(o.WorkDir, "bin", executableName())); !os.IsNotExist(err) {
		t.Fatal("copied a binary before ownership check")
	}
	if _, err := os.Stat(filepath.Join(o.WorkDir, "service.env")); !os.IsNotExist(err) {
		t.Fatal("wrote service.env before ownership check")
	}
}

func TestFailedInstallCheckKeepsExistingServiceEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FEISHU_BOT_HOME", filepath.Join(dir, "home"))
	configPath := writeInstallConfig(t, dir)
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	existing := "TEST_APP_SECRET=old-app\nTEST_MODEL_TOKEN=old-model\n"
	if err := os.WriteFile(filepath.Join(work, "service.env"), []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "new.env")
	if err := os.WriteFile(envPath, []byte("TEST_APP_SECRET=new-app\nTEST_MODEL_TOKEN=new-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldPrecheck := installPrecheck
	defer func() { installPrecheck = oldPrecheck }()
	for name, setup := range map[string]func() *fakeServiceManager{
		"precheck": func() *fakeServiceManager {
			installPrecheck = func(context.Context, core.Config) error { return errors.New("fixture precheck") }
			return &fakeServiceManager{platform: "test"}
		},
		"ownership": func() *fakeServiceManager {
			installPrecheck = func(context.Context, core.Config) error { return nil }
			return &fakeServiceManager{platform: "test", present: true, owned: false}
		},
	} {
		manager := setup()
		o := Options{Command: "daemon", Subcommand: "install", ConfigPath: configPath, ConfigExplicit: true, EnvFile: envPath, WorkDir: work, NoStart: true}
		if err := installWithManager(context.Background(), o, manager, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("%s: install succeeded", name)
		}
		if data, _ := os.ReadFile(filepath.Join(work, "service.env")); string(data) != existing {
			t.Fatalf("%s: service.env replaced by a failed install: %q", name, data)
		}
	}
}

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lines, []string{"three", "four"}) {
		t.Fatalf("tail lines = %#v", lines)
	}
	lines, err = tailLines(path, 0)
	if err != nil || len(lines) != 0 {
		t.Fatal(lines, err)
	}
}
