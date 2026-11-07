//go:build windows

package cli

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	name := "feishu-bot-test-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	manager := windowsManager{}
	if present, err := manager.Present(ctx, name); err != nil || present {
		t.Fatal("unique test task was not confirmed absent", present, err)
	}
	if snapshot, err := manager.Snapshot(ctx, name); err != nil || snapshot.Present {
		t.Fatal("missing task could not be snapshotted", snapshot.Present, err)
	}
	// The name was confirmed absent. Clean up a partially completed registration
	// too, and make cleanup failure visible rather than leaking a real task.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := manager.Uninstall(cleanupCtx, name); err != nil {
			t.Error("test task cleanup failed", err)
		}
	})
	if err := manager.Install(ctx, spec, false); err != nil {
		t.Fatal(err, toolDetail(err))
	}
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
	cancelled, cancelQuery := context.WithCancel(ctx)
	cancelQuery()
	if snapshot, err := manager.Snapshot(cancelled, name); !errors.Is(err, context.Canceled) || snapshot.Present {
		t.Fatal("cancelled snapshot was accepted", snapshot.Present, err)
	}
	if err := manager.Uninstall(cancelled, name); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled uninstall treated the task as absent", err)
	}
	if present, err := manager.Present(ctx, name); err != nil || !present {
		t.Fatal("cancelled query changed the original task", present, err)
	}
	if _, err := runTool(ctx, "schtasks", "/Change", "/TN", name, "/DISABLE"); err != nil {
		t.Fatal(err, toolDetail(err))
	}
	snapshot, err := manager.Snapshot(ctx, name)
	if err != nil || !snapshot.Present || taskSettingsEnabled(t, string(snapshot.Data)) {
		t.Fatal("disabled task snapshot failed", snapshot.Present, err)
	}
	if _, err := runTool(ctx, "schtasks", "/Change", "/TN", name, "/ENABLE"); err != nil {
		t.Fatal(err, toolDetail(err))
	}
	if err := manager.Restore(ctx, name, snapshot); err != nil {
		t.Fatal("old task restore failed", err, toolDetail(err))
	}
	restored, err := queryTaskXML(ctx, name)
	if err != nil || !taskOwned(restored, spec) || taskSettingsEnabled(t, restored) {
		t.Fatal("restored task lost its definition or disabled state", err)
	}
	if err := manager.Uninstall(ctx, name); err != nil {
		t.Fatal(err)
	}
	if present, err := manager.Present(ctx, name); err != nil || present {
		t.Fatal("deleted test task was not confirmed absent", present, err)
	}
}

func taskSettingsEnabled(t *testing.T, text string) bool {
	t.Helper()
	index := strings.Index(text, "<Task")
	if index < 0 {
		t.Fatal("task XML has no Task element")
	}
	var definition struct {
		Settings struct {
			Enabled *bool `xml:"Enabled"`
		} `xml:"Settings"`
	}
	if err := xml.Unmarshal([]byte(text[index:]), &definition); err != nil || definition.Settings.Enabled == nil {
		t.Fatal("task XML has no readable enabled state", err)
	}
	return *definition.Settings.Enabled
}
