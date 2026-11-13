package process

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminateWithGraceStopsProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "heartbeat")
	cmd := fixtureCommand(t, "ticker", marker)
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tree.TerminateWithGrace(done, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	stoppedHeartbeat(t, marker)
}
