package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Run executes a short command in an owned process tree. It is intended for
// version checks and workspace initialization; interactive native sessions use
// Prepare/Attach directly. Construct cmd with exec.Command, since Run owns its
// cancellation and confirms descendants even when the direct child exits first.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	return runOwned(ctx, cmd, false, nil)
}

// RunInteractive retains process-tree ownership while lending the foreground
// terminal to the child. It restores terminal ownership after confirmed cleanup.
func RunInteractive(ctx context.Context, cmd *exec.Cmd) error {
	return runOwned(ctx, cmd, true, nil)
}

// RunInteractiveWith calls beforeStart after every local terminal check and
// immediately before the child starts. A beforeStart error prevents the start.
func RunInteractiveWith(ctx context.Context, cmd *exec.Cmd, beforeStart func() error) error {
	return runOwned(ctx, cmd, true, beforeStart)
}

func runOwned(ctx context.Context, cmd *exec.Cmd, interactive bool, beforeStart func() error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = time.Second
	}
	Prepare(cmd)
	if interactive {
		restore, err := prepareInteractive(cmd)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, restore()) }()
	}
	if cmd.Err != nil {
		return cmd.Err
	}
	if beforeStart != nil {
		if err := beforeStart(); err != nil {
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	tree, err := Attach(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
		return err
	}
	defer tree.Close()
	exited := make(chan struct{})
	waited := make(chan error, 1)
	go func() { err := cmd.Wait(); waited <- err; close(exited) }()
	select {
	case result = <-waited:
	case <-ctx.Done():
		result = ctx.Err()
	}
	if err := tree.Terminate(exited); err != nil {
		return errors.Join(result, err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-exited:
		if result == nil {
			select {
			case result = <-waited:
			default:
			}
		}
		return result
	case <-timer.C:
		return errors.Join(result, fmt.Errorf("%w: direct child did not finish after tree termination", ErrUnconfirmed))
	}
}
