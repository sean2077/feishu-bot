package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

func writeStopRequest(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return errors.New("cannot access data directory")
	}
	data := []byte(fmt.Sprintf("{\"at\":%d}\n", time.Now().UnixMilli()))
	if err := os.WriteFile(stopRequestPath(dataDir), data, 0600); err != nil {
		return errors.New("cannot write stop request")
	}
	return nil
}

// stopRunning asks a live gateway to shut down through its control file and
// waits for both the lock and the recorded process to disappear. It never
// force-kills a process and never removes an unexplained stale lock.
func stopRunning(ctx context.Context, dataDir string) (alreadyStopped bool, err error) {
	lockPath := filepath.Join(dataDir, "gateway.lock")
	if _, statErr := os.Stat(lockPath); errors.Is(statErr, os.ErrNotExist) {
		_ = os.Remove(stopRequestPath(dataDir))
		return true, nil
	} else if statErr != nil {
		return false, errors.New("cannot read gateway.lock")
	}
	lock, err := workspace.ReadLock(dataDir)
	if err != nil {
		return false, errors.New("gateway.lock is unreadable; inspect it before stopping")
	}
	if !process.Alive(lock.PID) {
		return false, fmt.Errorf("gateway.lock records PID %d, which is not running; inspect the lock before removing it", lock.PID)
	}
	if err := writeStopRequest(dataDir); err != nil {
		return false, err
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		if _, statErr := os.Stat(lockPath); errors.Is(statErr, os.ErrNotExist) {
			exitDeadline := time.Now().Add(5 * time.Second)
			for process.Alive(lock.PID) && time.Now().Before(exitDeadline) {
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				time.Sleep(100 * time.Millisecond)
			}
			if process.Alive(lock.PID) {
				return false, errors.New("gateway.lock was released but the recorded process is still running")
			}
			_ = os.Remove(stopRequestPath(dataDir))
			return false, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if time.Now().After(deadline) {
			return false, errors.New("graceful shutdown did not finish within 45 seconds; gateway.lock remains")
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// reclaimStaleLock removes only an unowned lock whose PID is no longer running.
// It never kills a process or replays external work.
func reclaimStaleLock(dataDir string) error {
	lock, err := workspace.ReadLock(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(stopRequestPath(dataDir))
		return nil
	}
	if err != nil {
		return errors.New("gateway.lock is unreadable; inspect it before reclaiming")
	}
	if process.Alive(lock.PID) {
		return fmt.Errorf("gateway.lock records a running process (PID %d); refusing to reclaim", lock.PID)
	}
	if err := os.Remove(filepath.Join(dataDir, "gateway.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove stale gateway.lock")
	}
	_ = os.Remove(stopRequestPath(dataDir))
	return nil
}
