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

func TestStartTimeIsStableForCurrentProcess(t *testing.T) {
	first, ok := StartTime(os.Getpid())
	if !ok {
		t.Skip("process start time is unavailable on this platform")
	}
	if first <= 0 {
		t.Fatalf("start time = %d", first)
	}
	second, ok := StartTime(os.Getpid())
	if !ok || second != first {
		t.Fatalf("start time changed: %d != %d", first, second)
	}
	if _, ok := StartTime(0); ok {
		t.Fatal("PID 0 reported a start time")
	}
}
