package process

import (
	"context"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type residentMemoryCounters struct {
	Size, PageFaultCount                               uint32
	PeakWorkingSetSize, WorkingSetSize                 uintptr
	QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage       uintptr
	QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage uintptr
	PagefileUsage, PeakPagefileUsage, PrivateUsage     uintptr
}

var residentMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

func residentRows(ctx context.Context) ([]residentProcess, int, error) {
	root := os.Getpid()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, root, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snapshot, &entry)
	var rows []residentProcess
	for err == nil {
		if err := ctx.Err(); err != nil {
			return nil, root, err
		}
		rows = append(rows, residentProcess{int(entry.ProcessID), int(entry.ParentProcessID), -1})
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, root, err
	}
	included := residentDescendants(rows, root)
	for i := range rows {
		if err := ctx.Err(); err != nil {
			return nil, root, err
		}
		row := &rows[i]
		if !included[row.pid] {
			continue
		}
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(row.pid))
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			// The descendant exited after the snapshot (busy tools spawn many
			// short-lived processes); it no longer holds resident memory.
			// Other failures, such as access denied, leave the sample incomplete.
			row.bytes = 0
			continue
		}
		if err != nil {
			continue
		}
		var counters residentMemoryCounters
		counters.Size = uint32(unsafe.Sizeof(counters))
		ok, _, _ := residentMemoryInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
		windows.CloseHandle(handle)
		if ok != 0 {
			row.bytes = int64(counters.WorkingSetSize)
		}
	}
	return rows, root, nil
}
