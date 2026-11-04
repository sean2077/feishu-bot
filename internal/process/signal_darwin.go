//go:build darwin

package process

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

func (t *platformTree) signal(sig syscall.Signal) error {
	return confirmDarwinGroupSignal(syscall.Kill(-t.pid, sig), t.group.active, groupExitTimeout)
}

func confirmDarwinGroupSignal(err error, active func() (bool, error), timeout time.Duration) error {
	if !errors.Is(err, syscall.EPERM) {
		return err
	}
	// XNU makes exiting processes invisible to killpg1's proc_find before
	// setting SZOMB. sysctl can still return those records through zombproc,
	// so even a fresh snapshot may look active after kill returns EPERM.
	// Reuse the normal exit-confirmation deadline without resending a signal.
	// A failed query or a group that stays active must retain the original
	// permission error. See kern_exit.c's proc_prepareexit/proc_exit and
	// kern_proc.c's pgrp_iterate/proc_iterate in the XNU sources.
	if waitErr := waitGroupInactive(active, timeout); waitErr != nil {
		return errors.Join(err, fmt.Errorf("confirm process group exit after signal: %w", waitErr))
	}
	return nil
}
