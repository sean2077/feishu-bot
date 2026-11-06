//go:build linux

package process

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

func statFixture(pid, parent, group int, state byte, threads int, start uint64) []byte {
	fields := make([]string, 40)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = string(state)
	fields[1] = fmt.Sprint(parent)
	fields[2] = fmt.Sprint(group)
	fields[17] = fmt.Sprint(threads)
	fields[19] = fmt.Sprint(start)
	return []byte(fmt.Sprintf("%d (fixture (name) with space) %s\n", pid, strings.Join(fields, " ")))
}
func namespaceFixture() fstest.MapFS {
	return fstest.MapFS{
		"self/status": {Data: []byte("Pid:\t1000\nPPid:\t900\nNSpid:\t1000\t5\nNSpgid:\t900\t1\n")},
		"2000/status": {Data: []byte("Pid:\t2000\nPPid:\t1000\nNSpid:\t2000\t7\nNSpgid:\t2000\t7\n")},
		"2000/stat":   {Data: statFixture(2000, 1000, 2000, 'S', 1, 1234)},
		"2001/status": {Data: []byte("Pid:\t2001\nPPid:\t2000\nNSpid:\t2001\t8\nNSpgid:\t2000\t7\n")},
		"2001/stat":   {Data: statFixture(2001, 2000, 2000, 'S', 1, 1240)},
		// The same numeric namespace PID/PGID exists in a sibling namespace. It is
		// unrelated and must neither be captured nor block our group's cleanup.
		"3000/status": {Data: []byte("Pid:\t3000\nPPid:\t9000\nNSpid:\t3000\t7\nNSpgid:\t3000\t7\n")},
		"3000/stat":   {Data: statFixture(3000, 9000, 3000, 'D', 1, 1200)},
	}
}
func TestGroupIdentityTranslatesMountedProcNamespace(t *testing.T) {
	proc := namespaceFixture()
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil || group.group != 2000 || group.leader != 2000 || group.start != 1234 {
		t.Fatal(group, err)
	}
	if alive, err := group.active(); err != nil || !alive {
		t.Fatal("live group missed", alive, err)
	}
	proc["2000/stat"].Data = statFixture(2000, 1000, 2000, 'Z', 1, 1234)
	if alive, err := group.active(); err != nil || !alive {
		t.Fatal("leader death incorrectly confirmed descendant exit", alive, err)
	}
	proc["2001/stat"].Data = statFixture(2001, 1, 2000, 'Z', 1, 1240)
	if alive, err := group.active(); err != nil || alive {
		t.Fatal("zombies or sibling namespace blocked cleanup", alive, err)
	}
}
func TestGroupIdentityKeepsUninterruptibleMembersUnconfirmed(t *testing.T) {
	proc := namespaceFixture()
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	proc["2000/stat"].Data = statFixture(2000, 1000, 2000, 'Z', 1, 1234)
	proc["2001/stat"].Data = statFixture(2001, 1, 2000, 'D', 1, 1240)
	if err := waitGroupInactive(group.active, 0); err == nil {
		t.Fatal("successful signal treated as proof of process exit")
	}
	proc["2001/stat"].Data = statFixture(2001, 1, 2000, 'X', 1, 1240)
	if err := waitGroupInactive(group.active, 0); err != nil {
		t.Fatal(err)
	}
}
func TestGroupIdentityDoesNotAdoptRecycledLeader(t *testing.T) {
	proc := namespaceFixture()
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	proc["2000/stat"].Data = statFixture(2000, 1000, 2000, 'S', 1, 9999)
	if alive, err := group.active(); err != nil || alive {
		t.Fatal("recycled process was adopted", alive, err)
	}
}
func TestZombieLeaderWithLiveThreadsIsNotTerminated(t *testing.T) {
	proc := namespaceFixture()
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	proc["2000/stat"].Data = statFixture(2000, 1000, 2000, 'Z', 2, 1234)
	proc["2001/stat"].Data = statFixture(2001, 1, 2000, 'Z', 1, 1240)
	proc["2000/task/2000/stat"] = &fstest.MapFile{Data: statFixture(2000, 1000, 2000, 'Z', 2, 1234)}
	proc["2000/task/2002/stat"] = &fstest.MapFile{Data: statFixture(2002, 1000, 2000, 'D', 2, 1234)}
	if alive, err := group.active(); err != nil || !alive {
		t.Fatal("live thread hidden by zombie thread-group leader", alive, err)
	}
}
func TestNestedNamespaceMappingUsesCallersDepth(t *testing.T) {
	proc := namespaceFixture()
	proc["2000/status"].Data = []byte("Pid:\t2000\nPPid:\t1000\nNSpid:\t2000\t7\t1\nNSpgid:\t2000\t7\t1\n")
	if group, err := captureGroupFS(proc, 5, 7); err != nil || group.group != 2000 {
		t.Fatal(group, err)
	}
	if _, err := captureGroupFS(proc, 5, 1); err == nil {
		t.Fatal("used descendant's innermost PID instead of caller namespace")
	}
}
func TestCannotConfirmTerminationFromIncompleteProcData(t *testing.T) {
	proc := namespaceFixture()
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	proc["2001/stat"].Data = []byte("incomplete")
	// Remove the live leader so the scan must inspect the uncertain descendant.
	delete(proc, "2000/stat")
	if _, err := group.active(); err == nil {
		t.Fatal("unreadable process state silently treated as terminated")
	}
}
func TestTerminationWaitObservesActualExit(t *testing.T) {
	calls := 0
	err := waitGroupInactive(func() (bool, error) { calls++; return calls < 3, nil }, time.Second)
	if err != nil || calls != 3 {
		t.Fatal(calls, err)
	}
}

type failingProcFS struct {
	fstest.MapFS
	failures map[string]error
}

func (p failingProcFS) ReadFile(name string) ([]byte, error) {
	if err := p.failures[name]; err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	return p.MapFS.ReadFile(name)
}

func (p failingProcFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := p.failures[name]; err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	return p.MapFS.ReadDir(name)
}

func TestCaptureGroupIgnoresUnrelatedProcessThatExitedDuringRead(t *testing.T) {
	proc := failingProcFS{MapFS: namespaceFixture(), failures: map[string]error{"1500/status": syscall.ESRCH}}
	proc.MapFS["1500/status"] = &fstest.MapFile{Data: []byte("process exited after directory enumeration")}
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil || group == nil || group.group != 2000 || group.start != 1234 {
		t.Fatal("unrelated exiting process prevented tracking the live child", group, err)
	}
}

func TestCaptureGroupStillRequiresMatchedChildIdentity(t *testing.T) {
	proc := failingProcFS{MapFS: namespaceFixture(), failures: map[string]error{"2000/stat": syscall.ESRCH}}
	if group, err := captureGroupFS(proc, 5, 7); group != nil || !errors.Is(err, syscall.ESRCH) {
		t.Fatal("missing matched-child identity was treated as confirmed ownership", group, err)
	}
}

func TestExitedProcRecordDoesNotHideLiveGroupMember(t *testing.T) {
	proc := failingProcFS{MapFS: namespaceFixture(), failures: map[string]error{}}
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	proc.failures["2000/stat"] = syscall.ESRCH
	if alive, err := group.active(); err != nil || !alive {
		t.Fatal("vanished leader hid its live descendant", alive, err)
	}
	proc.failures["2001/stat"] = syscall.ESRCH
	if alive, err := group.active(); err != nil || alive {
		t.Fatal("fully exited group remained unconfirmed", alive, err)
	}
	proc.failures["2001/stat"] = syscall.EIO
	if _, err := group.active(); !errors.Is(err, syscall.EIO) {
		t.Fatal("unknown read failure was mistaken for an exited process", err)
	}
}

func TestExitedProcTaskRecordDoesNotHideLiveThread(t *testing.T) {
	proc := failingProcFS{MapFS: namespaceFixture(), failures: map[string]error{}}
	group, err := captureGroupFS(proc, 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	proc.MapFS["2000/stat"].Data = statFixture(2000, 1000, 2000, 'Z', 3, 1234)
	proc.MapFS["2001/stat"].Data = statFixture(2001, 1, 2000, 'Z', 1, 1240)
	proc.MapFS["2000/task/2000/stat"] = &fstest.MapFile{Data: statFixture(2000, 1000, 2000, 'Z', 3, 1234)}
	proc.MapFS["2000/task/2002/stat"] = &fstest.MapFile{Data: statFixture(2002, 1000, 2000, 'S', 1, 1241)}
	proc.MapFS["2000/task/2003/stat"] = &fstest.MapFile{Data: statFixture(2003, 1000, 2000, 'D', 1, 1242)}
	proc.failures["2000/task/2002/stat"] = syscall.ESRCH
	if alive, err := group.active(); err != nil || !alive {
		t.Fatal("exited task hid another live thread", alive, err)
	}
	proc.failures["2000/task"] = syscall.ESRCH
	if alive, err := group.active(); err != nil || alive {
		t.Fatal("disappeared thread group remained unconfirmed", alive, err)
	}
}
