//go:build !windows

package gateway

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// A detached process group remains addressable after its leader exits. Never
// use executable names or a recycled parent PID to discover descendants.
type processTree struct{ pid int }

func prepareProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func attachProcess(cmd *exec.Cmd) (*processTree, error) {
	return &processTree{pid: cmd.Process.Pid}, nil
}
func (t *processTree) terminate(exited <-chan struct{}) error {
	if t == nil || t.pid <= 0 {
		return nil
	}
	err := syscall.Kill(-t.pid, syscall.SIGTERM)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	timer := time.NewTimer(750 * time.Millisecond)
	select {
	case <-exited:
	case <-timer.C:
	}
	timer.Stop()
	// The leader may have exited while descendants still hold stdout open.
	err = syscall.Kill(-t.pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
func (t *processTree) close() {}
