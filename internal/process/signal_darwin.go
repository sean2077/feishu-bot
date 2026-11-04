//go:build darwin

package process

import (
	"errors"
	"fmt"
	"syscall"
)

func (t *platformTree) signal(sig syscall.Signal) error {
	return confirmDarwinGroupSignal(syscall.Kill(-t.pid, sig), t.group.active)
}

func confirmDarwinGroupSignal(err error, active func() (bool, error)) error {
	if !errors.Is(err, syscall.EPERM) {
		return err
	}
	// Darwin's killpg1 skips zombies, then returns EPERM when no signalable
	// member remains. The final descendant can exit between our active check
	// and kill, especially after SIGTERM. Accept only a fresh identity check
	// proving that no executing member remains; real permission errors stay
	// failures. See XNU bsd/kern/kern_sig.c, killpg1.
	alive, queryErr := active()
	if queryErr != nil {
		return errors.Join(err, fmt.Errorf("recheck process group after signal: %w", queryErr))
	}
	if !alive {
		return nil
	}
	return err
}
