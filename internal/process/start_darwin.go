//go:build darwin

package process

import "golang.org/x/sys/unix"

// StartTime returns the process creation time in nanoseconds. It is compared
// for equality only.
func StartTime(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid || p.Proc.P_starttime.Sec <= 0 {
		return 0, false
	}
	return p.Proc.P_starttime.Sec*1_000_000_000 + int64(p.Proc.P_starttime.Usec)*1_000, true
}
