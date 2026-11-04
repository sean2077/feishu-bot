//go:build windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestWindowsTaskRegistration is an opt-in integration test. It exercises the
// real schtasks path with a temporary task name and never touches the
// production feishu-bot task.
func TestWindowsTaskRegistration(t *testing.T) {
	if os.Getenv("FEISHU_BOT_TASK_TEST") != "1" {
		t.Skip("set FEISHU_BOT_TASK_TEST=1 to exercise schtasks")
	}
	dir := t.TempDir()
	name := "feishu-bot-test-" + strconv.Itoa(os.Getpid())
	spec := serviceSpec{
		Name: name, Dir: dir,
		Binary:     filepath.Join(dir, "feishu-bot.exe"),
		Config:     filepath.Join(dir, "config.json"),
		EnvFile:    filepath.Join(dir, "service.env"),
		LogFile:    filepath.Join(dir, "logs", "service.log"),
		LogMaxSize: 10, LogMaxBackups: 3,
	}
	username, err := windowsUser()
	if err != nil {
		t.Fatal(err)
	}
	if got := taskField(taskXML(spec, username), "Command"); got != spec.Binary {
		t.Fatalf("generated task command = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := windowsManager{}
	if err := manager.Install(ctx, spec, false); err != nil {
		t.Fatal(err, toolDetail(err))
	}
	defer func() { _ = manager.Uninstall(context.Background(), name) }()
	present, err := manager.Present(ctx, name)
	if err != nil || !present {
		t.Fatal("registered task is not present", err)
	}
	registered, err := queryTaskXML(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !taskOwned(registered, spec) {
		t.Fatalf("registered task was not recognized:\n%s", registered)
	}
}
