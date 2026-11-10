package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func upgradeFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FEISHU_BOT_HOME", filepath.Join(dir, "home"))
	path := writeInstallConfig(t, dir)
	env := filepath.Join(dir, "new.env")
	if err := os.WriteFile(env, []byte("TEST_APP_SECRET=new-app\nTEST_MODEL_TOKEN=new-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "candidate")
	if err := os.WriteFile(binary, []byte("candidate binary"), 0700); err != nil {
		t.Fatal(err)
	}
	old := installPrecheck
	installPrecheck = func(context.Context, core.Config) error { return nil }
	t.Cleanup(func() { installPrecheck = old })
	return Options{ConfigPath: path, ConfigExplicit: true, EnvFile: env, WorkDir: filepath.Join(dir, "work"), Binary: binary, NoStart: true}, dir, binary
}

func TestUpgradeStagesBinaryBeforeTouchingRunningService(t *testing.T) {
	o, dir, _ := upgradeFixture(t)
	data := filepath.Join(dir, "state")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(data, "gateway.lock")
	if err := os.WriteFile(lock, []byte("existing lock must remain untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	o.Binary = filepath.Join(dir, "missing")
	manager := &fakeServiceManager{platform: "test", present: true, owned: true}
	err := installWithManager(context.Background(), o, manager, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "executable source") {
		t.Fatalf("unexpected failure: %v", err)
	}
	if manager.installed || manager.restored || manager.started {
		t.Fatal("service changed before executable validation")
	}
	if b, _ := os.ReadFile(lock); string(b) != "existing lock must remain untouched" {
		t.Fatal("running service lock changed")
	}
}

func prepareUpgradeRegistrationRestore(t *testing.T) (Options, []string, *fakeServiceManager) {
	t.Helper()
	o, _, _ := upgradeFixture(t)
	meta, err := daemonMetaPath()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(o.WorkDir, "bin", executableName()), filepath.Join(o.WorkDir, "service.env"), meta}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("prior content"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return o, paths, &fakeServiceManager{platform: "test", present: true, owned: true, installErr: errors.New("registration rejected after write")}
}

// windowsTransientRename reports the hosted-Windows sharing violation seen when
// an antivirus or indexer briefly holds a just-written file. The caller must
// only retry after a confirmed rollback, never over a partial installation.
func windowsTransientRename(err error) bool {
	if runtime.GOOS != "windows" || err == nil {
		return false
	}
	return errors.Is(err, os.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "access is denied")
}

func TestUpgradeRegistrationFailureRestoresAllPublishedFiles(t *testing.T) {
	var paths []string
	var manager *fakeServiceManager
	var workDir string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var o Options
		o, paths, manager = prepareUpgradeRegistrationRestore(t)
		workDir = o.WorkDir
		err = installWithManager(context.Background(), o, manager, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !windowsTransientRename(err) || !strings.Contains(err.Error(), "previous installation restored") || attempt == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err == nil || !strings.Contains(err.Error(), "previous installation restored") {
		t.Fatal(err)
	}
	if !manager.restored || manager.started {
		t.Fatal("prior service definition not restored")
	}
	for _, path := range paths {
		if b, err := os.ReadFile(path); err != nil || string(b) != "prior content" {
			t.Fatalf("restore %s: %q %v", path, b, err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(workDir, ".feishu-bot-upgrade-*"))
	if len(backups) != 0 {
		t.Fatalf("completed rollback leaked recovery copies: %v", backups)
	}
}

func TestUpgradeMetadataFailureRollsBackNewInstallation(t *testing.T) {
	o, _, _ := upgradeFixture(t)
	meta, err := daemonMetaPath()
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeServiceManager{platform: "test"}
	manager.installHook = func() {
		// The service registration completed, but metadata publication now fails.
		if err := os.MkdirAll(meta, 0700); err != nil {
			t.Fatal(err)
		}
	}
	err = installWithManager(context.Background(), o, manager, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "previous installation restored") {
		t.Fatal(err)
	}
	if !manager.restored || manager.present || manager.started {
		t.Fatal("new registration was not undone")
	}
	for _, path := range []string{meta, filepath.Join(o.WorkDir, "service.env"), filepath.Join(o.WorkDir, "bin", executableName())} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial installation remains: %s %v", path, err)
		}
	}
}

func TestUpgradeStartFailureKeepsConsistentInstalledCandidate(t *testing.T) {
	o, _, _ := upgradeFixture(t)
	o.NoStart = false
	manager := &fakeServiceManager{platform: "test", startErr: errors.New("start outcome unknown")}
	err := installWithManager(context.Background(), o, manager, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "installation committed") {
		t.Fatal(err)
	}
	if manager.restored {
		t.Fatal("replaced files after an unconfirmed start")
	}
	meta, err := loadDaemonMeta()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(meta.BinaryPath); err != nil || string(b) != "candidate binary" {
		t.Fatal(string(b), err)
	}
	if b, err := os.ReadFile(meta.EnvFile); err != nil || !strings.Contains(string(b), "new-model") {
		t.Fatal(string(b), err)
	}
	backups, _ := filepath.Glob(filepath.Join(o.WorkDir, ".feishu-bot-upgrade-*"))
	if len(backups) != 1 {
		t.Fatal(backups)
	}
	if _, err := os.Stat(filepath.Join(backups[0], "recovery.json")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRollbackRestoresEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeServiceManager{platform: "test"}
	backup, err := prepareInstallBackup(context.Background(), dir, manager, path)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.cleanup()
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backup.rollback(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Fatal(string(b), err)
	}
}

type cancelledSnapshotManager struct {
	*fakeServiceManager
	cancel context.CancelFunc
}

func (m cancelledSnapshotManager) Snapshot(context.Context, string) (serviceDefinition, error) {
	m.cancel()
	return serviceDefinition{}, nil
}

func TestUpgradeRejectsSnapshotCompletedAfterCancellation(t *testing.T) {
	o, _, _ := upgradeFixture(t)
	oldBinary := filepath.Join(o.WorkDir, "bin", executableName())
	if err := os.MkdirAll(filepath.Dir(oldBinary), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldBinary, []byte("original executable"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := cancelledSnapshotManager{fakeServiceManager: &fakeServiceManager{present: true, owned: true}, cancel: cancel}
	err := installWithManager(ctx, o, manager, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("accepted a late snapshot result", err)
	}
	if manager.installed || manager.restored || manager.started {
		t.Fatal("modified registration after the snapshot deadline")
	}
	if content, err := os.ReadFile(oldBinary); err != nil || string(content) != "original executable" {
		t.Fatal("published files after the snapshot deadline", err)
	}
}
