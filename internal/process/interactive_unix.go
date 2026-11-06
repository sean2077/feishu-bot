//go:build !windows

package process

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"golang.org/x/sys/unix"
)

func prepareInteractive(cmd *exec.Cmd) (func() error,error) {
	input,ok:=cmd.Stdin.(*os.File)
	if !ok{return nil,errors.New("local session requires a foreground terminal")}
	fd:=int(input.Fd())
	group,err:=unix.IoctlGetInt(fd,unix.TIOCGPGRP)
	if err!=nil||group!=unix.Getpgrp(){return nil,errors.New("local session requires a foreground terminal")}
	cmd.SysProcAttr.Foreground=true
	cmd.SysProcAttr.Ctty=fd
	return func()error{
		// The parent is now in the background. Ignore only SIGTTOU around the
		// ioctl which returns the terminal to the original foreground group.
		ignored:=signal.Ignored(syscall.SIGTTOU)
		signal.Ignore(syscall.SIGTTOU)
		if !ignored {defer signal.Reset(syscall.SIGTTOU)}
		return unix.IoctlSetPointerInt(fd,unix.TIOCSPGRP,group)
	},nil
}
