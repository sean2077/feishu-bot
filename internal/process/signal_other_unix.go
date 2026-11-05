//go:build !windows && !darwin

package process

import "syscall"

func (t *platformTree) signal(sig syscall.Signal) error {
	return syscall.Kill(-t.pid, sig)
}
