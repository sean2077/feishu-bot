package process

import (
	"os"
	"testing"
)

func TestAliveCurrentProcess(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("current process was reported as not alive")
	}
}

func TestAliveInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1, 1 << 30} {
		if Alive(pid) {
			t.Fatalf("invalid PID %d was reported as alive", pid)
		}
	}
}
