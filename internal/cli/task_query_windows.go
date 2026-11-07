//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// schtasks uses the same exit status for a missing task, denied access and
// scheduler failures. Query the Task Scheduler COM interface instead: only
// GetTask's HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND) establishes absence.
// Registration and XML export still use schtasks; no script is installed.
func scheduledTaskPresent(ctx context.Context, name string) (bool, error) {
	return awaitTaskQuery(ctx, func(queryCtx context.Context) (bool, error) {
		return queryScheduledTask(queryCtx, name)
	})
}

var (
	taskOLE32       = windows.NewLazySystemDLL("ole32.dll")
	taskOLEAut32    = windows.NewLazySystemDLL("oleaut32.dll")
	taskCreate      = taskOLE32.NewProc("CoCreateInstance")
	taskAllocString = taskOLEAut32.NewProc("SysAllocStringLen")
	taskFreeString  = taskOLEAut32.NewProc("SysFreeString")
	taskClassID     = windows.GUID{Data1: 0x0f87369f, Data2: 0xa4e5, Data3: 0x4cfc, Data4: [8]byte{0xbd, 0x3e, 0x73, 0xe6, 0x15, 0x45, 0x72, 0xdd}}
	taskServiceID   = windows.GUID{Data1: 0x2faba4c7, Data2: 0x4da9, Data3: 0x4013, Data4: [8]byte{0x96, 0x97, 0x20, 0xcc, 0x3f, 0xd4, 0x0f, 0x85}}
)

// These vtable slots are the fixed IUnknown/IDispatch/Task Scheduler ABI from
// the Windows SDK's taskschd.h. All objects and BSTRs stay on one COM thread.
const (
	taskRelease        = 2
	taskGetIDsOfNames  = 5
	taskInvoke         = 6
	taskServiceFolder  = 7
	taskFolderGetTask  = 13
	taskFileNotFoundHR = 0x80070002
)

type taskCOMObject struct {
	vtable *[14]uintptr
}

func (p *taskCOMObject) release() {
	if p != nil {
		syscall.SyscallN(p.vtable[taskRelease], uintptr(unsafe.Pointer(p)))
	}
}

func queryScheduledTask(ctx context.Context, name string) (bool, error) {
	// COM has no context-aware GetTask. Cancellation returns to the caller via
	// scheduledTaskPresent while this read-only call finishes and releases its
	// objects on their owning thread; it can never publish a service change.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && err != syscall.Errno(1) {
		return false, fmt.Errorf("cannot initialize Task Scheduler query: %w", err)
	}
	defer windows.CoUninitialize() // S_OK and S_FALSE both require balancing.
	var service *taskCOMObject
	hr, _, _ := taskCreate.Call(uintptr(unsafe.Pointer(&taskClassID)), 0, windows.CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&taskServiceID)), uintptr(unsafe.Pointer(&service)))
	if uint32(hr) != 0 {
		return false, taskQueryError("create Task Scheduler", hr)
	}
	if service == nil {
		return false, errors.New("Task Scheduler returned no service interface")
	}
	defer service.release()
	if err := connectTaskService(service); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root, err := taskBSTR(`\`)
	if err != nil {
		return false, err
	}
	defer taskFreeString.Call(root)
	var folder *taskCOMObject
	hr, _, _ = syscall.SyscallN(service.vtable[taskServiceFolder], uintptr(unsafe.Pointer(service)), root, uintptr(unsafe.Pointer(&folder)))
	if uint32(hr) != 0 {
		return false, taskQueryError("open Task Scheduler root", hr)
	}
	if folder == nil {
		return false, errors.New("Task Scheduler returned no root folder")
	}
	defer folder.release()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path, err := taskBSTR(name)
	if err != nil {
		return false, err
	}
	defer taskFreeString.Call(path)
	var task *taskCOMObject
	hr, _, _ = syscall.SyscallN(folder.vtable[taskFolderGetTask], uintptr(unsafe.Pointer(folder)), path, uintptr(unsafe.Pointer(&task)))
	defer task.release()
	present, err := taskLookupStatus(uint32(hr))
	if err == nil && present && task == nil {
		return false, errors.New("Task Scheduler returned no task interface")
	}
	return present, err
}

func taskLookupStatus(hr uint32) (bool, error) {
	switch hr {
	case 0:
		return true, nil
	case taskFileNotFoundHR:
		return false, nil
	default:
		return false, taskQueryError("query scheduled task", uintptr(hr))
	}
}

func taskQueryError(action string, hr uintptr) error {
	return fmt.Errorf("cannot %s (HRESULT 0x%08x)", action, uint32(hr))
}

// Connect's four VARIANT arguments are all optional. IDispatch with no
// arguments avoids architecture-specific by-value VARIANT calling conventions.
func connectTaskService(service *taskCOMObject) error {
	var iidNull windows.GUID
	name, err := windows.UTF16PtrFromString("Connect")
	if err != nil {
		return err
	}
	var id int32
	hr, _, _ := syscall.SyscallN(service.vtable[taskGetIDsOfNames], uintptr(unsafe.Pointer(service)),
		uintptr(unsafe.Pointer(&iidNull)), uintptr(unsafe.Pointer(&name)), 1, 0, uintptr(unsafe.Pointer(&id)))
	if uint32(hr) != 0 {
		return taskQueryError("resolve Task Scheduler Connect", hr)
	}
	var params struct {
		args, namedArgs      uintptr
		argCount, namedCount uint32
	}
	hr, _, _ = syscall.SyscallN(service.vtable[taskInvoke], uintptr(unsafe.Pointer(service)), uintptr(id),
		uintptr(unsafe.Pointer(&iidNull)), 0, 1, uintptr(unsafe.Pointer(&params)), 0, 0, 0)
	if uint32(hr) != 0 {
		return taskQueryError("connect to Task Scheduler", hr)
	}
	return nil
}

func taskBSTR(text string) (uintptr, error) {
	value, err := windows.UTF16FromString(text)
	if err != nil {
		return 0, err
	}
	ptr, _, _ := taskAllocString.Call(uintptr(unsafe.Pointer(&value[0])), uintptr(len(value)-1))
	if ptr == 0 {
		return 0, errors.New("cannot allocate Task Scheduler query string")
	}
	return ptr, nil
}
