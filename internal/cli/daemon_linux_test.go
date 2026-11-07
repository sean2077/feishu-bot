//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type systemctlFixture struct {
	manager linuxManager
	dir     string
	state   string
	log     string
}

func newSystemctlFixture(t *testing.T) systemctlFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	f := systemctlFixture{manager: linuxManager{}, dir: dir, state: filepath.Join(dir, "state"), log: filepath.Join(dir, "commands")}
	t.Setenv("FEISHU_TEST_SYSTEMCTL_STATE", f.state)
	t.Setenv("FEISHU_TEST_SYSTEMCTL_LOG", f.log)
	t.Setenv("FEISHU_TEST_SYSTEMCTL_UNIT", f.manager.unitPath())
	t.Setenv("FEISHU_TEST_SYSTEMCTL_DISABLE_COPY", filepath.Join(dir, "disabled-unit"))
	t.Setenv("FEISHU_TEST_SYSTEMCTL_META_FAIL", "")
	t.Setenv("FEISHU_TEST_SYSTEMCTL_DISABLE_FAIL", "")
	t.Setenv("FEISHU_TEST_SYSTEMCTL_QUERY_FAIL", "")
	t.Setenv("FEISHU_TEST_SYSTEMCTL_LARGE_LIST", "")
	// Only the executable lookup is replaced. Install, Snapshot, Restore and
	// runTool are production code and operate on the fixture's actual files.
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FEISHU_TEST_SYSTEMCTL_LOG"
if [ "${1:-}" = --user ]; then shift; fi
action="$1"
shift
case "$action" in
list-unit-files)
  [ "$#" -eq 4 ] && [ "$1" = --no-legend ] && [ "$2" = --no-pager ] && [ "$3" = --full ] && [ "$4" = --plain ] || exit 64
  if [ -n "$FEISHU_TEST_SYSTEMCTL_QUERY_FAIL" ]; then echo 'query failed' >&2; exit 1; fi
  if [ -n "$FEISHU_TEST_SYSTEMCTL_LARGE_LIST" ]; then
    i=0
    while [ "$i" -lt 2000 ]; do printf 'other-long-service-name-%s.service enabled enabled\n' "$i"; i=$((i + 1)); done
  fi
  printf 'default.target static -\n'
  if [ -f "$FEISHU_TEST_SYSTEMCTL_UNIT" ] || [ -f "$FEISHU_TEST_SYSTEMCTL_STATE" ]; then
    printf 'feishu-bot.service enabled enabled\n'
  fi
  ;;
daemon-reload)
  [ "$#" -eq 0 ] || { echo 'daemon-reload takes no unit' >&2; exit 64; }
  if [ ! -f "$FEISHU_TEST_SYSTEMCTL_UNIT" ] && [ -f "$FEISHU_TEST_SYSTEMCTL_STATE" ] && [ "$(cat "$FEISHU_TEST_SYSTEMCTL_STATE")" = disabled ]; then
    rm "$FEISHU_TEST_SYSTEMCTL_STATE"
  fi
  ;;
is-enabled)
  [ "$#" -eq 1 ] && [ "$1" = feishu-bot.service ] || exit 64
  if [ -n "$FEISHU_TEST_SYSTEMCTL_QUERY_FAIL" ]; then echo 'query failed' >&2; exit 1; fi
  if [ -f "$FEISHU_TEST_SYSTEMCTL_STATE" ]; then state="$(cat "$FEISHU_TEST_SYSTEMCTL_STATE")"
  elif [ -f "$FEISHU_TEST_SYSTEMCTL_UNIT" ]; then state=disabled
  else
    # Old systemd versions return only a localized diagnostic here. First
    # installation and rollback removal must not depend on parsing it.
    echo 'unit query failed: localized missing-unit diagnostic' >&2
    exit 1
  fi
  printf '%s\n' "$state"
  case "$state" in
    enabled|enabled-runtime|static|indirect|linked|linked-runtime|alias|generated|transient) exit 0 ;;
    not-found) exit 4 ;;
    *) exit 1 ;;
  esac
  ;;
enable|disable)
  [ "$#" -eq 1 ] && [ "$1" = feishu-bot.service ] || exit 64
  if [ "$action" = disable ]; then
    if [ -n "$FEISHU_TEST_SYSTEMCTL_DISABLE_FAIL" ]; then echo 'disable rejected' >&2; exit 1; fi
    cp "$FEISHU_TEST_SYSTEMCTL_UNIT" "$FEISHU_TEST_SYSTEMCTL_DISABLE_COPY"
    printf 'disabled\n' > "$FEISHU_TEST_SYSTEMCTL_STATE"
  else
    printf 'enabled\n' > "$FEISHU_TEST_SYSTEMCTL_STATE"
    if [ -n "$FEISHU_TEST_SYSTEMCTL_META_FAIL" ] && [ ! -e "$FEISHU_TEST_SYSTEMCTL_META_FAIL.once" ]; then
      mkdir -p "$FEISHU_TEST_SYSTEMCTL_META_FAIL"
      : > "$FEISHU_TEST_SYSTEMCTL_META_FAIL.once"
    fi
  fi
  ;;
start|stop)
  [ "$#" -eq 1 ] && [ "$1" = feishu-bot.service ] || exit 64
  ;;
*) echo 'unexpected command' >&2; exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f systemctlFixture) setState(t *testing.T, state string) {
	t.Helper()
	if err := os.WriteFile(f.state, []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f systemctlFixture) writeUnit(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.manager.unitPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.manager.unitPath(), []byte(content), 0640); err != nil {
		t.Fatal(err)
	}
}

func (f systemctlFixture) commands(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestLinuxServiceCommandsSeparateManagerAndUnit(t *testing.T) {
	f := newSystemctlFixture(t)
	for _, manager := range []linuxManager{{}, {root: true}} {
		if err := manager.control(context.Background(), "daemon-reload"); err != nil {
			t.Fatal(err)
		}
		if err := manager.Start(context.Background(), serviceName); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"--user daemon-reload", "--user start feishu-bot.service", "daemon-reload", "start feishu-bot.service"}
	if got := f.commands(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestLinuxUpgradeRestoresEnablement(t *testing.T) {
	for _, prior := range []string{"enabled", "disabled", "absent"} {
		t.Run(prior, func(t *testing.T) {
			o, _, _ := upgradeFixture(t)
			o.Replace = true
			f := newSystemctlFixture(t)
			const old = "[Unit]\nDescription=original service\n[Service]\nExecStart=/example/old\n[Install]\nWantedBy=original.target\n"
			if prior != "absent" {
				f.writeUnit(t, old)
				f.setState(t, prior)
			}
			meta, err := daemonMetaPath()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("FEISHU_TEST_SYSTEMCTL_META_FAIL", meta)
			err = installWithManager(context.Background(), o, f.manager, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "previous installation restored") {
				t.Fatalf("metadata failure did not complete rollback: %v", err)
			}
			data, readErr := os.ReadFile(f.manager.unitPath())
			if prior == "absent" {
				missing, err := f.manager.missingDefinition(context.Background(), serviceName)
				if !errors.Is(readErr, os.ErrNotExist) || err != nil || !missing {
					t.Fatalf("new registration remains: %q %v missing=%t err=%v", data, readErr, missing, err)
				}
			} else {
				state, err := f.manager.enablement(context.Background(), serviceName)
				if err != nil {
					t.Fatal(err)
				}
				if readErr != nil || string(data) != old || state != prior {
					t.Fatalf("old registration not restored: %q %v state=%s", data, readErr, state)
				}
				info, err := os.Stat(f.manager.unitPath())
				if err != nil || info.Mode().Perm() != 0640 {
					t.Fatal("unit mode was not restored", err)
				}
			}
			// Disabling after writing the old definition would use the wrong
			// WantedBy/Alias/Also values. Assert the actual file seen by systemctl.
			disabled, err := os.ReadFile(filepath.Join(f.dir, "disabled-unit"))
			if err != nil || !strings.Contains(string(disabled), "run --service") || string(disabled) == old {
				t.Fatalf("disabled the wrong unit definition: %q %v", disabled, err)
			}
			for _, command := range f.commands(t) {
				if strings.Contains(command, "start ") || strings.Contains(command, "stop ") {
					t.Fatalf("changed process state after a pre-start registration failure: %s", command)
				}
			}
			backups, _ := filepath.Glob(filepath.Join(o.WorkDir, ".feishu-bot-upgrade-*"))
			if len(backups) != 0 {
				t.Fatalf("completed rollback retained copies: %v", backups)
			}
		})
	}
}

func TestLinuxUpgradeRejectsUnrestorableStatesBeforePublication(t *testing.T) {
	states := []string{"enabled-runtime", "masked", "masked-runtime", "static", "indirect", "linked", "linked-runtime", "alias", "generated", "transient", "unknown"}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			o, _, _ := upgradeFixture(t)
			o.Replace = true
			f := newSystemctlFixture(t)
			const old = "unchanged existing unit"
			f.writeUnit(t, old)
			f.setState(t, state)
			err := installWithManager(context.Background(), o, f.manager, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "cannot save existing service definition") {
				t.Fatalf("accepted unrestorable state %s: %v", state, err)
			}
			if data, err := os.ReadFile(f.manager.unitPath()); err != nil || string(data) != old {
				t.Fatal("changed existing unit", err)
			}
			for _, path := range []string{filepath.Join(o.WorkDir, "bin", executableName()), filepath.Join(o.WorkDir, "service.env")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("published installation file %s: %v", path, err)
				}
			}
			if commands := f.commands(t); !reflect.DeepEqual(commands, []string{"--user is-enabled feishu-bot.service"}) {
				t.Fatalf("ran commands after failed preflight: %q", commands)
			}
		})
	}
}

func TestLinuxSnapshotRequiresKnownDefinitionAndQuery(t *testing.T) {
	t.Run("outside-managed-path", func(t *testing.T) {
		f := newSystemctlFixture(t)
		f.setState(t, "enabled")
		if _, err := f.manager.Snapshot(context.Background(), serviceName); err == nil {
			t.Fatal("discarded an existing unmanaged definition")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		f := newSystemctlFixture(t)
		f.writeUnit(t, "target")
		target := filepath.Join(f.dir, "foreign-unit")
		if err := os.Rename(f.manager.unitPath(), target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, f.manager.unitPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.manager.Snapshot(context.Background(), serviceName); err == nil {
			t.Fatal("accepted a definition whose link identity cannot be restored")
		}
		if targetAfter, err := os.Readlink(f.manager.unitPath()); err != nil || targetAfter != target {
			t.Fatal("changed original symlink", err)
		}
	})
	t.Run("query-failure", func(t *testing.T) {
		f := newSystemctlFixture(t)
		t.Setenv("FEISHU_TEST_SYSTEMCTL_QUERY_FAIL", "1")
		if _, err := f.manager.Snapshot(context.Background(), serviceName); err == nil {
			t.Fatal("treated an unknown query failure as missing")
		}
	})
	t.Run("large-list", func(t *testing.T) {
		f := newSystemctlFixture(t)
		f.setState(t, "enabled") // Existing definition follows >64 KiB of other units.
		t.Setenv("FEISHU_TEST_SYSTEMCTL_LARGE_LIST", "1")
		if _, err := f.manager.Snapshot(context.Background(), serviceName); err == nil || !strings.Contains(err.Error(), "outside the managed path") {
			t.Fatal("a large unit inventory hid the existing definition", err)
		}
		missing := newSystemctlFixture(t)
		t.Setenv("FEISHU_TEST_SYSTEMCTL_LARGE_LIST", "1")
		if definition, err := missing.manager.Snapshot(context.Background(), serviceName); err != nil || definition.Present {
			t.Fatal("a large unit inventory blocked a first installation", definition, err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		f := newSystemctlFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.manager.Snapshot(ctx, serviceName); !errors.Is(err, context.Canceled) {
			t.Fatalf("treated cancellation as missing: %v", err)
		}
	})
}

func TestLinuxUpgradeKeepsRecoveryWhenDisableFails(t *testing.T) {
	o, _, _ := upgradeFixture(t)
	o.Replace = true
	f := newSystemctlFixture(t)
	f.writeUnit(t, "original unit")
	f.setState(t, "disabled")
	meta, err := daemonMetaPath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FEISHU_TEST_SYSTEMCTL_META_FAIL", meta)
	t.Setenv("FEISHU_TEST_SYSTEMCTL_DISABLE_FAIL", "1")
	err = installWithManager(context.Background(), o, f.manager, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "rollback incomplete") {
		t.Fatal("claimed complete rollback after disable failed", err)
	}
	backups, _ := filepath.Glob(filepath.Join(o.WorkDir, ".feishu-bot-upgrade-*", "recovery.json"))
	if len(backups) != 1 {
		t.Fatal("lost recovery metadata", backups)
	}
	if data, err := os.ReadFile(backups[0]); err != nil || !strings.Contains(string(data), `"enablement": "disabled"`) {
		t.Fatal("lost prior enablement", err)
	}
}

func TestLinuxRollbackLeavesUntouchedForeignEnablement(t *testing.T) {
	f := newSystemctlFixture(t)
	f.writeUnit(t, "[Install]\nAlso=other.service\nWantedBy=custom.target\n")
	f.setState(t, "enabled")
	backup, err := prepareInstallBackup(context.Background(), f.dir, f.manager)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.cleanup()
	if err := backup.rollback(context.Background(), f.manager); err != nil {
		t.Fatal(err)
	}
	for _, command := range f.commands(t) {
		if command != "--user is-enabled feishu-bot.service" {
			t.Fatalf("altered untouched foreign registration: %s", command)
		}
	}
}
