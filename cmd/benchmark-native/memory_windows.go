package main

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PROCESS_MEMORY_COUNTERS_EX is pointer-sized on both supported Windows
// architectures. PrivateUsage is committed private bytes, not Working Set.
type processMemoryCounters struct {
	Size, PageFaultCount                               uint32
	PeakWorkingSetSize, WorkingSetSize                 uintptr
	QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage       uintptr
	QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage uintptr
	PagefileUsage, PeakPagefileUsage, PrivateUsage     uintptr
}

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

func platformMemoryRows() ([]processMemory, int, string, error) {
	const method = "Win32 Toolhelp32 process tree + GetProcessMemoryInfo WorkingSetSize and PrivateUsage (bytes)"
	root := os.Getpid()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, root, method, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snapshot, &entry)
	rows := []processMemory{}
	for err == nil {
		rows = append(rows, processMemory{pid: int(entry.ProcessID), ppid: int(entry.ParentProcessID), rss: -1, private: -1, name: windows.UTF16ToString(entry.ExeFile[:])})
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, root, method, err
	}
	included := descendantPIDs(rows, root)
	for i := range rows {
		row := &rows[i]
		if !included[row.pid] {
			continue
		}
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(row.pid))
		if err != nil {
			continue
		}
		var counters processMemoryCounters
		counters.Size = uint32(unsafe.Sizeof(counters))
		ok, _, _ := getProcessMemoryInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
		windows.CloseHandle(handle)
		if ok != 0 {
			row.rss, row.private = int64(counters.WorkingSetSize), int64(counters.PrivateUsage)
		}
	}
	return rows, root, method, nil
}
