//go:build darwin

package process

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestDarwinSignalPermissionErrorRequiresConfirmedExit(t *testing.T) {
	queryFailure := errors.New("cannot read process identity")
	for _, tc := range []struct {
		name    string
		signal  error
		alive   bool
		query   error
		want    error
		queries int
	}{
		{name: "exit between snapshot and signal", signal: syscall.EPERM, queries: 1},
		{name: "live member permission denied", signal: syscall.EPERM, alive: true, want: syscall.EPERM, queries: 1},
		{name: "query failed", signal: syscall.EPERM, query: queryFailure, want: syscall.EPERM, queries: 1},
		{name: "successful signal"},
		{name: "missing group", signal: syscall.ESRCH, want: syscall.ESRCH},
		{name: "other signal error", signal: syscall.EINVAL, want: syscall.EINVAL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries := 0
			err := confirmDarwinGroupSignal(tc.signal, func() (bool, error) {
				queries++
				return tc.alive, tc.query
			}, 0)
			if !errors.Is(err, tc.want) || queries != tc.queries {
				t.Fatalf("signal result = %v, queries = %d; want %v, %d", err, queries, tc.want, tc.queries)
			}
			if tc.query != nil && !errors.Is(err, tc.query) {
				t.Fatal("lost the identity query failure", err)
			}
		})
	}
}

func TestDarwinSignalWaitsForExitStateToSettle(t *testing.T) {
	queries := 0
	err := confirmDarwinGroupSignal(syscall.EPERM, func() (bool, error) {
		queries++
		// An exiting process can remain non-SZOMB in multiple sysctl
		// snapshots after proc_find can no longer find it for signalling.
		return queries < 3, nil
	}, time.Second)
	if err != nil || queries != 3 {
		t.Fatalf("exit transition result = %v, queries = %d; want success after 3 snapshots", err, queries)
	}
}

func TestDarwinSignalExitWaitPreservesLaterQueryFailure(t *testing.T) {
	queryFailure := errors.New("cannot read process identity during exit")
	queries := 0
	err := confirmDarwinGroupSignal(syscall.EPERM, func() (bool, error) {
		queries++
		if queries == 1 {
			return true, nil
		}
		return false, queryFailure
	}, time.Second)
	if !errors.Is(err, syscall.EPERM) || !errors.Is(err, queryFailure) || queries != 2 {
		t.Fatalf("unconfirmed exit lost errors: %v, queries = %d", err, queries)
	}
}

func TestDarwinSignalRechecksUnreapedZombieGroup(t *testing.T) {
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
	// Keep the exited child unreaped so its numerical PGID cannot be reused.
	// Its group exists but has no executing member when the signal arrives.
	if err = waitGroupInactive(g.active, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	tree := platformTree{pid: cmd.Process.Pid, group: g}
	if err = tree.signal(syscall.SIGKILL); err != nil {
		t.Fatal("exited group was reported as a permission failure", err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
