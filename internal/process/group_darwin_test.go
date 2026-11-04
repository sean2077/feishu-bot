//go:build darwin

package process

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func darwinProcess(pid, pgid int32, second int64, state int8) unix.KinfoProc {
	return unix.KinfoProc{
		Proc:  unix.ExternProc{P_pid: pid, P_starttime: unix.Timeval{Sec: second, Usec: 123}, P_stat: state},
		Eproc: unix.Eproc{Pgid: pgid},
	}
}

func TestDarwinGroupIdentitySnapshots(t *testing.T) {
	original := darwinProcess(4100, 4100, 100, 2)
	child := darwinProcess(4101, 4100, 101, 3)
	reused := darwinProcess(4100, 4100, 200, 2)
	zombie := darwinProcess(4101, 4100, 101, 5)
	for _, tc := range []struct {
		name            string
		leader, members []unix.KinfoProc
		alive, wantErr  bool
	}{
		{name: "live leader", leader: []unix.KinfoProc{original}, members: []unix.KinfoProc{original}, alive: true},
		{name: "orphan survives reaped leader", members: []unix.KinfoProc{child}, alive: true},
		{name: "gone group"},
		{name: "only zombies", members: []unix.KinfoProc{zombie}},
		{name: "zombie and live descendant", members: []unix.KinfoProc{zombie, child}, alive: true},
		{name: "reused leader PID", leader: []unix.KinfoProc{reused}, members: []unix.KinfoProc{reused}},
		{name: "reuse between snapshots", leader: []unix.KinfoProc{original}, members: []unix.KinfoProc{child, reused}},
		{name: "wrong group", members: []unix.KinfoProc{darwinProcess(5000, 5000, 100, 2)}, wantErr: true},
		{name: "wrong PID query", leader: []unix.KinfoProc{child}, wantErr: true},
		{name: "ambiguous PID query", leader: []unix.KinfoProc{original, original}, wantErr: true},
		{name: "invalid process state", members: []unix.KinfoProc{darwinProcess(4100, 4100, 100, 0)}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := groupIdentity{pid: 4100, start: original.Proc.P_starttime}
			alive, err := g.activeSnapshot(tc.leader, tc.members)
			if alive != tc.alive || (err != nil) != tc.wantErr {
				t.Fatalf("active = %t, error = %v; want active %t, error %t", alive, err, tc.alive, tc.wantErr)
			}
		})
	}
}

func TestDarwinCaptureOwnedChildBeforeReap(t *testing.T) {
	cmd := fixtureCommand(t, "normal", "")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	g, err := captureGroup(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if g.pid != cmd.Process.Pid || g.start.Sec <= 0 {
		t.Fatal("captured another process or an empty start time")
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if alive, err := g.active(); err != nil || alive {
		t.Fatalf("reaped child remained active: %t, %v", alive, err)
	}
	if _, err = captureGroup(os.Getpid()); err == nil {
		t.Fatal("accepted the current process as an owned direct child")
	}
}
