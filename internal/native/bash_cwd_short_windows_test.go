//go:build windows

package native

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

func windowsShortPath(t *testing.T, path string) string {
	t.Helper()
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetShortPathNameW")
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 32768)
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || n >= uintptr(len(buf)) {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func TestBashCwdPrefixMatchesWindowsShortName(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "workspace")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	short := windowsShortPath(t, work)
	if short == "" || short == work {
		t.Skip("8.3 short path is unavailable")
	}
	if !bashCwdMatches(short, false, work, home) {
		t.Fatalf("short path %q did not match %q", short, work)
	}
}
