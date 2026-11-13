//go:build !windows

package process

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminateWithGraceLetsHandlerRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "finally")
	cmd := fixtureCommand(t, "graceful", marker)
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tree, err := Attach(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	defer tree.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if err = tree.TerminateWithGrace(done, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "finally" {
		t.Fatalf("graceful handler did not run: %q %v", data, err)
	}
}
