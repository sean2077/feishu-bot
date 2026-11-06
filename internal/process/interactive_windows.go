//go:build windows

package process

import (
	"errors"
	"os"
	"os/exec"
	"golang.org/x/sys/windows"
)

func prepareInteractive(cmd *exec.Cmd) (func()error,error) {
	input,ok:=cmd.Stdin.(*os.File)
	if !ok{return nil,errors.New("local session requires a console terminal")}
	var mode uint32
	if err:=windows.GetConsoleMode(windows.Handle(input.Fd()),&mode);err!=nil{return nil,errors.New("local session requires a console terminal")}
	cmd.SysProcAttr.HideWindow=false
	// Preserve suspended-start assignment to our Job Object, but share console
	// control events so Ctrl+C can stop the owned local handoff as a whole.
	cmd.SysProcAttr.CreationFlags &^= 0x00000200
	return func()error{return nil},nil
}
