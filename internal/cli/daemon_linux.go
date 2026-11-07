//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type linuxManager struct {
	root bool
}

func platformServiceManager() (serviceManager, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, errors.New("systemd is required for daemon management on Linux")
	}
	return linuxManager{root: os.Geteuid() == 0}, nil
}

func (m linuxManager) Platform() string {
	if m.root {
		return "systemd-system"
	}
	return "systemd-user"
}

func (m linuxManager) unitPath() string {
	if m.root {
		return "/etc/systemd/system/feishu-bot.service"
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "systemd", "user", "feishu-bot.service")
}

func (m linuxManager) command(ctx context.Context, args ...string) (string, error) {
	if !m.root {
		args = append([]string{"--user"}, args...)
	}
	return runTool(ctx, "systemctl", args...)
}

func (m linuxManager) control(ctx context.Context, action string, units ...string) error {
	args := append([]string{action}, units...)
	if _, err := m.command(ctx, args...); err != nil {
		if detail := toolDetail(err); detail != "" {
			return fmt.Errorf("systemctl %s failed: %s", action, detail)
		}
		return fmt.Errorf("systemctl %s failed", action)
	}
	return nil
}

func (m linuxManager) Check(ctx context.Context, spec serviceSpec, replace bool) error {
	data, err := os.ReadFile(m.unitPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect systemd unit")
	}
	if string(data) == systemdUnit(spec, !m.root) {
		return nil
	}
	if replace {
		return nil
	}
	return errors.New("a feishu-bot systemd unit already exists with a different definition; stop/remove it or rerun with --replace")
}

func (m linuxManager) Install(ctx context.Context, spec serviceSpec, replace bool) error {
	if err := m.Check(ctx, spec, replace); err != nil {
		return err
	}
	path := m.unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errors.New("cannot create systemd unit directory")
	}
	unit := systemdUnit(spec, !m.root)
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		return errors.New("cannot write systemd unit")
	}
	if err := m.control(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := m.control(ctx, "enable", serviceName+".service"); err != nil {
		return err
	}
	verified, err := os.ReadFile(path)
	if err != nil || string(verified) != unit {
		return errors.New("systemd unit could not be verified")
	}
	return nil
}

func (m linuxManager) Start(ctx context.Context, name string) error {
	return m.control(ctx, "start", name+".service")
}

func (m linuxManager) Uninstall(ctx context.Context, name string) error {
	_ = m.control(ctx, "disable", name+".service")
	_ = m.control(ctx, "stop", name+".service")
	path := m.unitPath()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove systemd unit")
	}
	_ = m.control(ctx, "daemon-reload")
	return nil
}

func (m linuxManager) Present(ctx context.Context, name string) (bool, error) {
	_, err := os.Stat(m.unitPath())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, errors.New("cannot inspect systemd unit")
}

func (m linuxManager) Snapshot(ctx context.Context, name string) (serviceDefinition, error) {
	info, err := os.Lstat(m.unitPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return serviceDefinition{}, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return serviceDefinition{}, errors.New("cannot preserve a systemd unit that is not a regular file; manage this service definition explicitly before installing")
	}
	if errors.Is(err, os.ErrNotExist) {
		missing, err := m.missingDefinition(ctx, name)
		if err != nil {
			return serviceDefinition{}, err
		}
		if !missing {
			return serviceDefinition{}, errors.New("systemd unit exists outside the managed path; cannot preserve its definition automatically")
		}
		return serviceDefinition{}, nil
	}
	state, stateErr := m.enablement(ctx, name)
	if stateErr != nil {
		return serviceDefinition{}, stateErr
	}
	if state != "enabled" && state != "disabled" {
		return serviceDefinition{}, fmt.Errorf("cannot preserve systemd enablement state %q; only regular enabled or disabled units support automatic rollback", state)
	}
	data, err := os.ReadFile(m.unitPath())
	if err != nil {
		return serviceDefinition{}, err
	}
	return serviceDefinition{Present: true, Data: data, Mode: info.Mode().Perm(), Enablement: state}, nil
}

func (m linuxManager) Restore(ctx context.Context, name string, definition serviceDefinition) error {
	if definition.Present && definition.Enablement != "enabled" && definition.Enablement != "disabled" {
		return errors.New("recovery definition has no restorable systemd enablement state")
	}
	current, err := os.ReadFile(m.unitPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	state := "not-found"
	if errors.Is(err, os.ErrNotExist) {
		missing, err := m.missingDefinition(ctx, name)
		if err != nil {
			return err
		}
		if !missing {
			return errors.New("systemd unit moved outside the managed path during installation")
		}
	} else {
		state, err = m.enablement(ctx, name)
		if err != nil {
			return err
		}
	}
	// Nothing reached service registration if the original file and state are
	// still present. In particular, do not disable an untouched foreign unit's
	// Also= dependencies while rolling back an earlier file publication failure.
	if definition.Present && bytes.Equal(current, definition.Data) && state == definition.Enablement {
		return os.Chmod(m.unitPath(), definition.Mode)
	}
	if state != "enabled" && state != "disabled" && state != "not-found" {
		return fmt.Errorf("cannot restore unexpected systemd state %q", state)
	}
	// Disable while the candidate definition is still installed, so its links
	// are removed using its own [Install] section rather than the old unit's.
	if state == "enabled" {
		if err := m.control(ctx, "disable", name+".service"); err != nil {
			return err
		}
	}
	if !definition.Present {
		if err := os.Remove(m.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := m.control(ctx, "daemon-reload"); err != nil {
			return err
		}
		missing, err := m.missingDefinition(ctx, name)
		if err != nil {
			return err
		}
		if !missing {
			return errors.New("systemd registration remains after rollback")
		}
		return nil
	}
	if err := writePrivate(m.unitPath(), definition.Data); err != nil {
		return err
	}
	if err := os.Chmod(m.unitPath(), definition.Mode); err != nil {
		return err
	}
	if err := m.control(ctx, "daemon-reload"); err != nil {
		return err
	}
	if definition.Enablement == "enabled" {
		if err := m.control(ctx, "enable", name+".service"); err != nil {
			return err
		}
	}
	state, err = m.enablement(ctx, name)
	if err != nil {
		return err
	}
	if state != definition.Enablement {
		return fmt.Errorf("systemd enablement was not restored: got %q, want %q", state, definition.Enablement)
	}
	return nil
}

func (m linuxManager) missingDefinition(ctx context.Context, name string) (bool, error) {
	// Older systemd versions report a missing is-enabled target only as a
	// localized error. An exact-name list-unit-files query can also return a
	// failure status for no matches. Require a successful complete listing;
	// runTool rejects truncated output, so a missing tail cannot prove absence.
	output, err := m.command(ctx, "list-unit-files", "--no-legend", "--no-pager", "--full", "--plain")
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, fmt.Errorf("cannot inspect existing systemd definitions: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 || filepath.Ext(fields[0]) == "" || strings.ContainsRune(fields[0], '\x1b') {
			return false, errors.New("cannot interpret systemd unit-file listing")
		}
		if fields[0] == name+".service" {
			return false, nil
		}
	}
	return true, nil
}

func (m linuxManager) enablement(ctx context.Context, name string) (string, error) {
	output, err := m.command(ctx, "is-enabled", name+".service")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	state := strings.TrimSpace(output)
	if err != nil {
		// is-enabled intentionally returns a nonzero status for these states.
		// Execution, cancellation and unrecognized query failures remain errors.
		var failure *toolError
		if !errors.As(err, &failure) || failure.Code <= 0 || (state != "disabled" && state != "not-found" && state != "masked" && state != "masked-runtime") {
			return "", fmt.Errorf("cannot inspect systemd enablement: %w", err)
		}
	}
	return state, nil
}
