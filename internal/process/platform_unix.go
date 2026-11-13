//go:build !windows

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

type platformTree struct {
	pid   int
	group *groupIdentity
}

const groupExitTimeout = 5 * time.Second

func prepareCommand(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// Alive reports whether a process exists. EPERM means the process exists but
// belongs to another user, which is treated as alive conservatively.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
func attachCommand(cmd *exec.Cmd) (*platformTree, error) {
	group, err := captureGroup(cmd.Process.Pid)
	if err != nil {
		// The child has not been reaped and its freshly created group is still owned.
		// Fail startup rather than operating without a verifiable process identity.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil, fmt.Errorf("cannot track child process group: %w", err)
	}
	return &platformTree{pid: cmd.Process.Pid, group: group}, nil
}
func (t *platformTree) terminate(exited <-chan struct{}, grace time.Duration) error {
	if t == nil || t.pid <= 0 {
		return nil
	}
	if grace <= 0 {
		grace = 750 * time.Millisecond
	}
	alive, err := t.group.active()
	if err != nil {
		return fmt.Errorf("process group termination unconfirmed: %w", err)
	}
	if !alive {
		return nil
	}
	err = t.signal(syscall.SIGTERM)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("send SIGTERM to process group %d: %w", t.pid, err)
	}
	timer := time.NewTimer(grace)
	select {
	case <-exited:
	case <-timer.C:
	}
	timer.Stop()
	// Recheck ownership before the second signal: a vanished group must not be
	// confused with a later, unrelated process that reused the numerical PID.
	alive, err = t.group.active()
	if err != nil {
		return fmt.Errorf("process group termination unconfirmed: %w", err)
	}
	if !alive {
		return nil
	}
	err = t.signal(syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("send SIGKILL to process group %d: %w", t.pid, err)
	}
	if err = waitGroupInactive(t.group.active, groupExitTimeout); err != nil {
		return fmt.Errorf("process group termination unconfirmed: %w", err)
	}
	return nil
}
func (t *platformTree) close() {}
func waitGroupInactive(active func() (bool, error), timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := active()
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("live descendants remained after termination deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
