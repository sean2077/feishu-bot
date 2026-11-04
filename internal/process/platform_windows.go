//go:build windows

package process

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var createJob = kernel32.NewProc("CreateJobObjectW")
var setJob = kernel32.NewProc("SetInformationJobObject")
var assignJob = kernel32.NewProc("AssignProcessToJobObject")
var queryJob = kernel32.NewProc("QueryInformationJobObject")
var terminateJob = kernel32.NewProc("TerminateJobObject")
var openProcess = kernel32.NewProc("OpenProcess")
var waitForSingleObject = kernel32.NewProc("WaitForSingleObject")
var createSnapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
var firstThread = kernel32.NewProc("Thread32First")
var nextThread = kernel32.NewProc("Thread32Next")
var openThread = kernel32.NewProc("OpenThread")
var resumeThread = kernel32.NewProc("ResumeThread")
var closeHandle = kernel32.NewProc("CloseHandle")

type jobBasicLimit struct {
	PerProcessUserTime, PerJobUserTime           int64
	LimitFlags                                   uint32
	MinimumWorkingSetSize, MaximumWorkingSetSize uintptr
	ActiveProcessLimit                           uint32
	Affinity                                     uintptr
	PriorityClass, SchedulingClass               uint32
}
type ioCounters struct{ ReadOperationCount, WriteOperationCount, OtherOperationCount, ReadTransferCount, WriteTransferCount, OtherTransferCount uint64 }
type jobExtendedLimit struct {
	BasicLimitInformation                                                        jobBasicLimit
	IoInfo                                                                       ioCounters
	ProcessMemoryLimit, JobMemoryLimit, PeakProcessMemoryUsed, PeakJobMemoryUsed uintptr
}
type threadEntry struct {
	Size, Usage, ThreadID, OwnerPID uint32
	BasePriority, DeltaPriority     int32
	Flags                           uint32
}
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, PeriodUserTime, PeriodKernelTime               int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses uint32
}
type platformTree struct{ job uintptr }

func winError(name string, err error) error {
	if err == syscall.Errno(0) {
		err = syscall.EINVAL
	}
	return fmt.Errorf("%s: %w", name, err)
}

// Alive reports whether a process exists. Access-denied is treated as alive so
// callers remain conservative around locks owned by the same account.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, _, err := openProcess.Call(0x1000|0x00100000, 0, uintptr(pid)) // QUERY_LIMITED_INFORMATION | SYNCHRONIZE
	if handle == 0 {
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.ERROR_ACCESS_DENIED {
			return true
		}
		return false
	}
	defer closeHandle.Call(handle)
	result, _, _ := waitForSingleObject.Call(handle, 0)
	return result == 0x102 // WAIT_TIMEOUT
}
func prepareCommand(cmd *exec.Cmd) {
	// Suspend before the first instruction: all descendants enter the owned Job,
	// including a CLI that exits immediately after spawning a child.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000004 | 0x00000200}
}
func attachCommand(cmd *exec.Cmd) (tree *platformTree, err error) {
	job, _, e := createJob.Call(0, 0)
	if job == 0 {
		_ = cmd.Process.Kill()
		return nil, winError("CreateJobObject", e)
	}
	tree = &platformTree{job: job}
	defer func() {
		if err != nil {
			_ = cmd.Process.Kill()
			tree.close()
		}
	}()
	limits := jobExtendedLimit{}
	limits.BasicLimitInformation.LimitFlags = 0x00002000 // KILL_ON_JOB_CLOSE
	r, _, e := setJob.Call(job, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if r == 0 {
		return tree, winError("SetInformationJobObject", e)
	}
	process, _, e := openProcess.Call(0x0100|0x0001|0x0400, 0, uintptr(cmd.Process.Pid))
	if process == 0 {
		return tree, winError("OpenProcess", e)
	}
	defer closeHandle.Call(process)
	r, _, e = assignJob.Call(job, process)
	if r == 0 {
		return tree, winError("AssignProcessToJobObject", e)
	}
	snap, _, e := createSnapshot.Call(0x00000004, 0)
	if snap == ^uintptr(0) {
		return tree, winError("CreateToolhelp32Snapshot", e)
	}
	defer closeHandle.Call(snap)
	entry := threadEntry{Size: uint32(unsafe.Sizeof(threadEntry{}))}
	r, _, e = firstThread.Call(snap, uintptr(unsafe.Pointer(&entry)))
	resumed := false
	for r != 0 {
		if entry.OwnerPID == uint32(cmd.Process.Pid) {
			th, _, te := openThread.Call(0x0002, 0, uintptr(entry.ThreadID))
			if th == 0 {
				return tree, winError("OpenThread", te)
			}
			previous, _, te := resumeThread.Call(th)
			closeHandle.Call(th)
			if previous == 0xffffffff {
				return tree, winError("ResumeThread", te)
			}
			resumed = true
		}
		r, _, _ = nextThread.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}
	if !resumed {
		return tree, fmt.Errorf("Child startup thread not found; process remained suspended")
	}
	return tree, nil
}
func (t *platformTree) terminate(_ <-chan struct{}) error {
	if t == nil || t.job == 0 {
		return nil
	}
	r, _, err := terminateJob.Call(t.job, 1)
	if r == 0 {
		return winError("TerminateJobObject", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var accounting jobAccounting
		r, _, err = queryJob.Call(t.job, 1, uintptr(unsafe.Pointer(&accounting)), unsafe.Sizeof(accounting), 0)
		if r == 0 {
			return winError("QueryInformationJobObject", err)
		}
		if accounting.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Windows native process job termination unconfirmed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (t *platformTree) close() {
	if t != nil && t.job != 0 {
		closeHandle.Call(t.job)
		t.job = 0
	}
}
