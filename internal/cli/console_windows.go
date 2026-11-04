//go:build windows

package cli

import "syscall"

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var freeConsole = kernel32.NewProc("FreeConsole")

// detachConsole closes the console attached to this process, if any. Scheduled
// Tasks start console-subsystem programs without a terminal; detaching keeps a
// hidden task from showing a console window. Other processes sharing the
// console are unaffected.
func detachConsole() {
	_, _, _ = freeConsole.Call()
}
