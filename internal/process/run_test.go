package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "process-fixture" {
		processFixture(os.Args[2:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func processFixture(args []string) {
	switch args[0] {
	case "normal":
		fmt.Print("version fixture")
		return
	case "ticker":
		signal.Ignore(syscall.SIGTERM)
		for {
			_ = os.WriteFile(args[1], []byte(time.Now().Format(time.RFC3339Nano)), 0600)
			time.Sleep(10 * time.Millisecond)
		}
	case "graceful":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		<-ch
		_ = os.WriteFile(args[1], []byte("finally"), 0600)
	case "hold", "orphan", "closed-pipes":
		child := exec.Command(os.Args[0], "process-fixture", "ticker", args[1])
		if args[0] != "closed-pipes" {
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
		}
		if child.Start() != nil {
			os.Exit(21)
		}
		for i := 0; i < 1000; i++ {
			if _, err := os.Stat(args[1]); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if args[0] != "hold" {
			return
		}
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Second)
		}
	default:
		os.Exit(22)
	}
}
func fixtureCommand(t *testing.T, mode, path string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exec.Command(exe, "process-fixture", mode, path)
}
func stoppedHeartbeat(t *testing.T, path string) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("confirmed termination left a running descendant")
	}
}
func TestRunNormalAndCancelledBeforeStart(t *testing.T) {
	cmd := fixtureCommand(t, "normal", "")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := Run(context.Background(), cmd); err != nil || out.String() != "version fixture" {
		t.Fatal(out.String(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd = fixtureCommand(t, "normal", "")
	if err := Run(ctx, cmd); !errors.Is(err, context.Canceled) || cmd.Process != nil {
		t.Fatal("cancelled command started", err)
	}
}
func TestRunCancellationConfirmsWholeTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := fixtureCommand(t, "hold", path)
	result := make(chan error, 1)
	go func() { result <- Run(ctx, cmd) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnconfirmed) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not return")
	}
	stoppedHeartbeat(t, path)
}
func TestRunCleansDescendantsAfterParentExits(t *testing.T) {
	for _, mode := range []string{"orphan", "closed-pipes"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "heartbeat")
			cmd := fixtureCommand(t, mode, path)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			cmd.WaitDelay = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := Run(ctx, cmd)
			if errors.Is(err, ErrUnconfirmed) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if mode == "orphan" && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatal("retained pipe was not reported", err)
			}
			if mode == "closed-pipes" && err != nil {
				t.Fatal(err)
			}
			stoppedHeartbeat(t, path)
		})
	}
}
