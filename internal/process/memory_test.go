package process

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestResidentTreeIncludesDescendantsAndRejectsUnknownMemory(t *testing.T) {
	rows := []residentProcess{{10, 1, 100}, {11, 10, 200}, {12, 11, 300}, {20, 1, 9000}}
	if n, err := sumResidentTree(rows, 10); err != nil || n != 600 {
		t.Fatal(n, err)
	}
	rows[2].bytes = -1
	if _, err := sumResidentTree(rows, 10); err == nil {
		t.Fatal("partial process tree treated as a complete RSS sample")
	}
	if _, err := sumResidentTree(rows, 99); err == nil {
		t.Fatal("missing root accepted")
	}
}

func TestResidentTreeObservesRunningProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("platform collector unsupported")
	}
	// The collector documents a process snapshot, not an atomic tree census.
	// A short-lived descendant can exit between snapshot and memory lookup, so
	// one incomplete sample does not mean this live process has no RSS. Retry
	// only until a complete positive sample is available.
	deadline := time.Now().Add(2 * time.Second)
	var lastBytes int64
	var lastErr error
	for {
		n, err := ResidentTreeBytes(context.Background())
		if err == nil && n > 0 {
			return
		}
		lastBytes, lastErr = n, err
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("RSS=%d after retries: %v", lastBytes, lastErr)
}
