//go:build darwin

package process

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Darwin's kern.proc records expose the leader's start time and group members
// without /proc or CGO. A PGID remains reserved while its group exists, so a
// different process with the leader's PID means the original group is gone.
type groupIdentity struct {
	pid   int
	start unix.Timeval
}

func captureGroup(pid int) (*groupIdentity, error) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	if int(p.Proc.P_pid) != pid || int(p.Eproc.Ppid) != os.Getpid() || int(p.Eproc.Pgid) != pid || p.Proc.P_starttime.Sec <= 0 {
		return nil, fmt.Errorf("child process group identity unavailable before reap")
	}
	return &groupIdentity{pid: pid, start: p.Proc.P_starttime}, nil
}

func (g *groupIdentity) active() (bool, error) {
	// The slice API returns an empty result for a reaped leader. The single
	// process API reports EIO for that case, which is not a query failure here.
	leader, err := unix.SysctlKinfoProcSlice("kern.proc.pid", g.pid)
	if err != nil {
		return false, err
	}
	members, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", g.pid)
	if err != nil {
		return false, err
	}
	return g.activeSnapshot(leader, members)
}

func (g *groupIdentity) activeSnapshot(leader, members []unix.KinfoProc) (bool, error) {
	if len(leader) > 1 {
		return false, fmt.Errorf("ambiguous process identity")
	}
	if len(leader) == 1 {
		if int(leader[0].Proc.P_pid) != g.pid {
			return false, fmt.Errorf("process identity query returned another PID")
		}
		if leader[0].Proc.P_starttime != g.start {
			return false, nil
		}
	}
	live := false
	for _, p := range members {
		if p.Proc.P_pid <= 0 || int(p.Eproc.Pgid) != g.pid || p.Proc.P_stat < 1 || p.Proc.P_stat > 5 {
			return false, fmt.Errorf("invalid process group snapshot")
		}
		// The PID can be reused between the two queries. Check it again in the
		// group snapshot before authorizing a signal to that numerical PGID.
		if int(p.Proc.P_pid) == g.pid && p.Proc.P_starttime != g.start {
			return false, nil
		}
		// SZOMB (5 in Darwin's sys/proc.h) is waiting for its parent to reap
		// it; unlike an executing descendant, it cannot react to a signal.
		if p.Proc.P_stat != 5 {
			live = true
		}
	}
	return live, nil
}
