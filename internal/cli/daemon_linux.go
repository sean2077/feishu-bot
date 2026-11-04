//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func (m linuxManager) control(ctx context.Context, action string) error {
	args := []string{}
	if !m.root {
		args = append(args, "--user")
	}
	args = append(args, action, serviceName+".service")
	if _, err := runTool(ctx, "systemctl", args...); err != nil {
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
	if err := m.control(ctx, "enable"); err != nil {
		return err
	}
	verified, err := os.ReadFile(path)
	if err != nil || string(verified) != unit {
		return errors.New("systemd unit could not be verified")
	}
	return nil
}

func (m linuxManager) Start(ctx context.Context, name string) error {
	return m.control(ctx, "start")
}

func (m linuxManager) Uninstall(ctx context.Context, name string) error {
	_ = m.control(ctx, "disable")
	_ = m.control(ctx, "stop")
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
