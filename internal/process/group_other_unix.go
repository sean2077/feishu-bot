//go:build !windows && !linux && !darwin

package process

import (
	"errors"
	"syscall"
)

type groupIdentity struct{ pid int }

func captureGroup(pid int) (*groupIdentity, error) { return &groupIdentity{pid: pid}, nil }
func (g *groupIdentity) active() (bool, error) {
	err := syscall.Kill(-g.pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return err == nil, err
}
