package process

import (
	"context"
	"runtime"
	"testing"
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
	n, err := ResidentTreeBytes(context.Background())
	if err != nil || n <= 0 {
		t.Fatalf("RSS=%d: %v", n, err)
	}
}
