//go:build linux

package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This exercises real terminal job control without requiring the test runner
// itself to have a controlling terminal. The fixture supervisor owns a private
// session and lends its PTY to the child through the production RunInteractive.
func TestInteractivePTYForegroundAndRestoration(t *testing.T) {
	for _, mode := range []string{"normal", "cancel", "start-error"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			master, slave := openInteractivePTY(t)
			defer master.Close()
			defer slave.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInteractivePTYHelper$")
			cmd.Env = interactivePTYEnvironment("supervisor", mode, dir)
			cmd.Stdin = slave
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			// Fixed-size reads in the helpers keep each line with its intended
			// reader; buffered read-ahead would consume the restored parent's line.
			if _, err := master.Write([]byte("child-line\nparent-line\n")); err != nil {
				t.Fatal(err)
			}
			defer cleanupInteractivePTYGroup(dir)
			if err := cmd.Run(); err != nil {
				_ = unix.SetNonblock(int(master.Fd()), true)
				var terminal [4096]byte
				if n, _ := unix.Read(int(master.Fd()), terminal[:]); n > 0 {
					output.Write(terminal[:n])
				}
				t.Fatalf("PTY %s failed: %v\n%s", mode, err, output.String())
			}
			if !strings.Contains(output.String(), "foreground restored and readable") {
				t.Fatal("fixture did not verify restored terminal ownership", output.String())
			}
		})
	}
}

func openInteractivePTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal("real PTY allocation failed", err)
	}
	master := os.NewFile(uintptr(fd), "interactive-pty-master")
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatal(err)
	}
	return master, slave
}

func interactivePTYEnvironment(role, mode, dir string) []string {
	env := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "FEISHU_PTY_") {
			env = append(env, value)
		}
	}
	return append(env, "FEISHU_PTY_ROLE="+role, "FEISHU_PTY_MODE="+mode, "FEISHU_PTY_DIR="+dir)
}

func TestInteractivePTYHelper(t *testing.T) {
	role := os.Getenv("FEISHU_PTY_ROLE")
	if role == "" {
		return
	}
	mode, dir := os.Getenv("FEISHU_PTY_MODE"), os.Getenv("FEISHU_PTY_DIR")
	if role == "child" {
		interactivePTYChild(t, mode, dir)
		return
	}
	if role != "supervisor" {
		t.Fatal("unexpected fixture role")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer readyWriter.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInteractivePTYHelper$")
	cmd.Env = interactivePTYEnvironment("child", mode, dir)
	cmd.ExtraFiles = []*os.File{readyWriter}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdin, os.Stdin
	if mode == "start-error" {
		cmd = exec.Command(filepath.Join(dir, "missing-executable"))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdin, os.Stdin
	}
	if mode == "cancel" {
		go func() {
			var signal [1]byte
			if _, err := io.ReadFull(ready, signal[:]); err == nil {
				cancel()
			}
		}()
	}
	err = RunInteractive(ctx, cmd)
	switch mode {
	case "normal":
		if err != nil {
			t.Fatal("interactive normal exit", err)
		}
	case "cancel":
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnconfirmed) {
			t.Fatal("interactive cancellation did not confirm cleanup", err)
		}
	case "start-error":
		if err == nil || errors.Is(err, ErrUnconfirmed) {
			t.Fatal("failed launch result", err)
		}
	}
	group, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	if err != nil || group != unix.Getpgrp() {
		t.Fatal("foreground terminal was not returned to supervisor", group, err)
	}
	expected := "parent-line\n"
	if mode == "start-error" {
		expected = "child-line\n"
	}
	line := make([]byte, len(expected))
	if _, err := io.ReadFull(os.Stdin, line); err != nil || string(line) != expected {
		t.Fatal("restored supervisor could not read the terminal", string(line), err)
	}
	if mode != "start-error" {
		stoppedHeartbeat(t, filepath.Join(dir, "heartbeat"))
	}
	fmt.Println("foreground restored and readable")
}

func interactivePTYChild(t *testing.T, mode, dir string) {
	t.Helper()
	group, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	if err != nil || group != unix.Getpgrp() || group != os.Getpid() {
		t.Fatal("child did not own the foreground process group", group, err)
	}
	// Preserve the group identity for test cleanup even if a regression makes
	// the outer fixture time out. Never kill a numerically reused process group.
	statData, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	stat, err := parseStat(statData)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := json.Marshal(interactivePTYIdentity{PID: os.Getpid(), Leader: stat.pid, Group: stat.group, Start: stat.start})
	if err := os.WriteFile(filepath.Join(dir, "group.json"), identity, 0600); err != nil {
		t.Fatal(err)
	}
	line := make([]byte, len("child-line\n"))
	if _, err := io.ReadFull(os.Stdin, line); err != nil || string(line) != "child-line\n" {
		t.Fatal("foreground child could not read terminal input", string(line), err)
	}
	path := filepath.Join(dir, "heartbeat")
	descendant := exec.Command(os.Args[0], "process-fixture", "ticker", path)
	descendant.Stdin, descendant.Stdout, descendant.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := descendant.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned descendant did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if mode == "cancel" {
		signal.Ignore(syscall.SIGTERM)
	}
	ready := os.NewFile(3, "interactive-ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	ready.Close()
	if mode == "cancel" {
		for {
			time.Sleep(time.Hour)
		}
	}
}

type interactivePTYIdentity struct {
	PID, Leader, Group int
	Start              uint64
}

func cleanupInteractivePTYGroup(dir string) {
	data, err := os.ReadFile(filepath.Join(dir, "group.json"))
	if err != nil {
		return
	}
	var identity interactivePTYIdentity
	if json.Unmarshal(data, &identity) != nil || identity.PID <= 0 || identity.Leader <= 0 || identity.Group <= 0 || identity.Start == 0 {
		return
	}
	group := &groupIdentity{proc: os.DirFS("/proc"), leader: identity.Leader, group: identity.Group, start: identity.Start}
	tree := &Tree{platform: &platformTree{pid: identity.PID, group: group}}
	exited := make(chan struct{})
	close(exited)
	_ = tree.Terminate(exited)
	tree.Close()
}
