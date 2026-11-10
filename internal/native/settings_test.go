package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

const nativeSettingsBody = `{
  "env": {"EXAMPLE_PROFILE": "team"},
  "permissions": {
    "allow": ["Bash(uv run --no-sync example-tool sub:*)"],
    "ask": ["Bash(uv run --no-sync example-tool doc *--input*)"],
    "deny": ["Bash(curl:*)", "Read(./.secrets/**)"]
  }
}`

func writeNativeSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "project-settings.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadNativeSettings(t *testing.T, path string) (core.Project, config.ProjectSettings) {
	t.Helper()
	p := core.Project{ID: "p", SettingsFile: path}
	c := core.Config{AppSecretEnv: "APP_SECRET", Model: core.ModelConfig{TokenEnv: "MODEL_TOKEN"}, Projects: []core.Project{p}}
	loaded, err := config.LoadProjectSettings(c, p)
	if err != nil {
		t.Fatal(err)
	}
	return p, loaded
}

func nativeSettingsWorkspace(t *testing.T, mode string) core.Workspace {
	t.Helper()
	root := t.TempDir()
	w := core.Workspace{Root: root, Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Inbound: filepath.Join(root, "inbound"), NativeConfig: mode}
	for _, dir := range []string{w.Work, w.Home, w.Inbound} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

type privateSettingsDocument struct {
	Env         map[string]string   `json:"env"`
	Permissions map[string][]string `json:"permissions"`
	AutoMode    json.RawMessage     `json:"autoMode"`
}

func readPrivateSettings(t *testing.T, path string) privateSettingsDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc privateSettingsDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestPrivateSettingsPassesAutoModeThroughBothModes(t *testing.T) {
	path := writeNativeSettings(t, `{
  "env": {"EXAMPLE_PROFILE": "team"},
  "autoMode": {
    "environment": "classifier guidance",
    "allow": ["Bash(echo:*)"],
    "soft_deny": ["git push"],
    "futureField": {"nested": true}
  }
}`)
	p, loaded := loadNativeSettings(t, path)
	for _, mode := range []string{"isolated", "inherit"} {
		t.Run(mode, func(t *testing.T) {
			w := nativeSettingsWorkspace(t, mode)
			generated, err := privateSettings(w, p, map[string]string{"ANTHROPIC_AUTH_TOKEN": "token"}, &loaded)
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(generated)
			doc := readPrivateSettings(t, generated)
			var autoMode map[string]any
			if err := json.Unmarshal(doc.AutoMode, &autoMode); err != nil {
				t.Fatal(err)
			}
			if autoMode["environment"] != "classifier guidance" || autoMode["futureField"] == nil {
				t.Fatalf("autoMode not preserved: %#v", autoMode)
			}
			if allow, _ := autoMode["allow"].([]any); len(allow) != 1 || allow[0] != "Bash(echo:*)" {
				t.Fatalf("autoMode allow not preserved: %#v", autoMode)
			}
		})
	}
}

func TestPrivateSettingsMergesModelEnvironmentForInherit(t *testing.T) {
	path := writeNativeSettings(t, nativeSettingsBody)
	p, loaded := loadNativeSettings(t, path)
	w := nativeSettingsWorkspace(t, "inherit")
	modelEnv := map[string]string{"ANTHROPIC_AUTH_TOKEN": "model-token", "ANTHROPIC_MODEL": "model"}
	generated, err := privateSettings(w, p, modelEnv, &loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(generated)
	doc := readPrivateSettings(t, generated)
	if doc.Env["EXAMPLE_PROFILE"] != "team" || doc.Env["ANTHROPIC_AUTH_TOKEN"] != "model-token" || doc.Env["ANTHROPIC_MODEL"] != "model" {
		t.Fatalf("merged env = %#v", doc.Env)
	}
	if got := doc.Permissions["allow"]; len(got) != 1 || got[0] != "Bash(uv run --no-sync example-tool sub:*)" {
		t.Fatalf("allow rules = %#v", got)
	}
	if got := doc.Permissions["deny"]; len(got) != 2 || got[1] != "Read(./.secrets/**)" {
		t.Fatalf("deny rules = %#v", got)
	}
}

func TestPrivateSettingsIsolatedOmitsModelEnvironment(t *testing.T) {
	path := writeNativeSettings(t, nativeSettingsBody)
	p, loaded := loadNativeSettings(t, path)
	w := nativeSettingsWorkspace(t, "isolated")
	modelEnv := map[string]string{"ANTHROPIC_AUTH_TOKEN": "model-token", "ANTHROPIC_MODEL": "model"}
	generated, err := privateSettings(w, p, modelEnv, &loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(generated)
	doc := readPrivateSettings(t, generated)
	if doc.Env["EXAMPLE_PROFILE"] != "team" {
		t.Fatalf("file env missing: %#v", doc.Env)
	}
	for key := range modelEnv {
		if _, ok := doc.Env[key]; ok {
			t.Fatalf("isolated settings file contains model environment %q", key)
		}
	}
	if doc.Permissions["ask"][0] != "Bash(uv run --no-sync example-tool doc *--input*)" {
		t.Fatalf("permissions not preserved: %#v", doc.Permissions)
	}
}

func TestPrivateSettingsWithoutSettingsFileKeepsExistingBehavior(t *testing.T) {
	w := nativeSettingsWorkspace(t, "isolated")
	if path, err := privateSettings(w, core.Project{ID: "p"}, nil, nil); err != nil || path != "" {
		t.Fatalf("isolated without settings file = %q, %v", path, err)
	}
	w = nativeSettingsWorkspace(t, "inherit")
	generated, err := privateSettings(w, core.Project{ID: "p"}, map[string]string{"ANTHROPIC_AUTH_TOKEN": "token"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(generated)
	doc := readPrivateSettings(t, generated)
	if doc.Env["ANTHROPIC_AUTH_TOKEN"] != "token" || doc.Permissions != nil {
		t.Fatalf("inherit default settings = %#v", doc)
	}
}

func TestPrivateSettingsInheritAllowsRoomRootUnderUserHome(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "state", "rooms", "room")
	w := core.Workspace{
		Root:         root,
		Work:         filepath.Join(home, "workspace"),
		Home:         home,
		Inbound:      filepath.Join(root, "inbound"),
		ConfigDir:    filepath.Join(home, ".claude"),
		NativeConfig: "inherit",
	}
	for _, dir := range []string{w.Work, w.Inbound, w.ConfigDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	modelEnv := map[string]string{"ANTHROPIC_AUTH_TOKEN": "model-token"}
	generated, err := privateSettings(w, core.Project{ID: "p"}, modelEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(generated)
	if !core.Inside(w.Home, generated) {
		t.Fatalf("inherit settings were not under the user home: %s", generated)
	}
	if core.Inside(w.Work, generated) || core.Inside(w.ConfigDir, generated) {
		t.Fatalf("inherit settings were placed in the work or config directory: %s", generated)
	}
	w.Root = w.Work
	if _, err := privateSettings(w, core.Project{ID: "p"}, modelEnv, nil); err == nil {
		t.Fatal("inherit settings accepted a file inside the work directory")
	}
	w.Root = w.ConfigDir
	if _, err := privateSettings(w, core.Project{ID: "p"}, modelEnv, nil); err == nil {
		t.Fatal("inherit settings accepted a file inside the native config directory")
	}
	w.NativeConfig = "isolated"
	w.Root = w.Home
	p, loaded := loadNativeSettings(t, writeNativeSettings(t, nativeSettingsBody))
	if _, err := privateSettings(w, p, nil, &loaded); err == nil {
		t.Fatal("isolated settings accepted a file inside the managed home")
	}
}

func TestPrivateSettingsRequiresIsolatedWorkspaceRoot(t *testing.T) {
	path := writeNativeSettings(t, nativeSettingsBody)
	p, loaded := loadNativeSettings(t, path)
	w := core.Workspace{Work: t.TempDir(), Home: t.TempDir(), Inbound: t.TempDir(), NativeConfig: "isolated"}
	if _, err := privateSettings(w, p, nil, &loaded); err == nil {
		t.Fatal("isolated settings accepted an empty workspace root")
	}
	w.Root = w.Work
	if _, err := privateSettings(w, p, nil, &loaded); err == nil {
		t.Fatal("isolated settings accepted a file inside the native work directory")
	}
}

func TestIsolatedSettingsFileIsOutsideNativeFileScope(t *testing.T) {
	path := writeNativeSettings(t, nativeSettingsBody)
	p, loaded := loadNativeSettings(t, path)
	w, _, _ := windowsScopeFixture(t, "isolated")
	generated, err := privateSettings(w.workspace, p, nil, &loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(generated)
	if core.Inside(w.workspace.Work, generated) || core.Inside(w.workspace.Home, generated) {
		t.Fatalf("private settings were placed inside the native work/home: %s", generated)
	}
	if !hookDenied(t, w, "Read", args{"file_path": generated}, "") {
		t.Fatal("isolated file scope allowed reading the generated settings file")
	}
}

func TestWorkerRejectsInvalidProjectSettingsWithoutStarting(t *testing.T) {
	f := newFixture(t)
	f.config.Projects[0].SettingsFile = filepath.Join(t.TempDir(), "missing.json")
	worker, err := NewFactory(f.config, nil)(context.Background(), core.Room{ProjectID: "p"}, f.session, f.workspace, nil)
	if err == nil || worker != nil {
		t.Fatalf("invalid settings file started a worker: %v", err)
	}
	files, globErr := filepath.Glob(filepath.Join(f.workspace.Root, "native-settings-*.json"))
	if globErr != nil || len(files) != 0 {
		t.Fatalf("invalid settings left temporary files: %#v %v", files, globErr)
	}
}

func TestWorkerUsesAndCleansProjectSettings(t *testing.T) {
	f := newFixture(t)
	f.config.Projects[0].SettingsFile = writeNativeSettings(t, nativeSettingsBody)
	f.start()
	w := f.worker.(*worker)
	if w.settings == "" {
		t.Fatal("worker did not create a private settings file")
	}
	doc := readPrivateSettings(t, w.settings)
	if doc.Env["EXAMPLE_PROFILE"] != "team" || len(doc.Permissions["deny"]) != 2 {
		t.Fatalf("worker settings = %#v", doc)
	}
	if !strings.Contains(strings.Join(w.cmd.Args, " "), "--settings "+w.settings) {
		t.Fatalf("worker args omitted --settings: %v", w.cmd.Args)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.settings); !os.IsNotExist(err) {
		t.Fatal("private settings file survived worker stop")
	}
}

func TestWorkerReloadsProjectSettingsOnNextLaunch(t *testing.T) {
	f := newFixture(t)
	path := writeNativeSettings(t, `{"env":{"EXAMPLE_PROFILE":"first"}}`)
	f.config.Projects[0].SettingsFile = path
	f.start()
	first := f.worker.(*worker)
	if doc := readPrivateSettings(t, first.settings); doc.Env["EXAMPLE_PROFILE"] != "first" {
		t.Fatalf("first settings = %#v", doc.Env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := first.Stop(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := os.WriteFile(path, []byte(`{"env":{"EXAMPLE_PROFILE":"second"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.start()
	second := f.worker.(*worker)
	if doc := readPrivateSettings(t, second.settings); doc.Env["EXAMPLE_PROFILE"] != "second" {
		t.Fatalf("second settings = %#v", doc.Env)
	}
}

func TestInteractiveRejectsInvalidProjectSettings(t *testing.T) {
	f := newFixture(t)
	s := f.session
	s.CCID = core.NewID()
	s.RoomKey = "room"
	f.config.Projects[0].SettingsFile = filepath.Join(t.TempDir(), "missing.json")
	if _, _, err := interactiveCommand(f.config, core.Room{Key: "room", ProjectID: "p"}, s, f.workspace); err == nil {
		t.Fatal("interactive accepted an invalid settings file")
	}
	files, _ := filepath.Glob(filepath.Join(f.workspace.Root, "native-settings-*.json"))
	if len(files) != 0 {
		t.Fatalf("interactive invalid settings left temporary files: %#v", files)
	}
}

func TestInteractiveUsesAndCleansProjectSettings(t *testing.T) {
	f := newFixture(t)
	s := f.session
	s.CCID = core.NewID()
	s.RoomKey = "room"
	f.config.Projects[0].SettingsFile = writeNativeSettings(t, nativeSettingsBody)
	cmd, cleanup, err := interactiveCommand(f.config, core.Room{Key: "room", ProjectID: "p"}, s, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--settings ") {
		t.Fatalf("interactive args omitted --settings: %v", cmd.Args)
	}
	path := ""
	for i, arg := range cmd.Args {
		if arg == "--settings" && i+1 < len(cmd.Args) {
			path = cmd.Args[i+1]
		}
	}
	if path == "" {
		t.Fatalf("interactive args omitted the settings path: %v", cmd.Args)
	}
	doc := readPrivateSettings(t, path)
	if doc.Env["EXAMPLE_PROFILE"] != "team" || len(doc.Permissions["allow"]) != 1 {
		t.Fatalf("interactive settings = %#v", doc)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("interactive cleanup left the settings file")
	}
}
