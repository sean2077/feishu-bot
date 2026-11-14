//go:build linux

package process

import "testing"

func TestStartIdentityExcludesWallClock(t *testing.T) {
	a := startIdentity("boot-a", 12345)
	if b := startIdentity("boot-a", 12345); b != a {
		t.Fatalf("same boot identity changed: %d != %d", a, b)
	}
	if startIdentity("boot-b", 12345) == a {
		t.Fatal("different boots produced the same identity")
	}
	if startIdentity("boot-a", 12346) == a {
		t.Fatal("different starttime ticks produced the same identity")
	}
	if startIdentity("", 12345) != 12345 {
		t.Fatal("missing boot ID must leave raw ticks intact")
	}
	for _, boot := range []string{"boot-a", "boot-b", ""} {
		if startIdentity(boot, 12345) < 0 {
			t.Fatalf("start identity is negative for boot %q", boot)
		}
	}
}

// startIdentity has no wall-clock input. This test documents the invariant that
// a btime correction cannot change the identity for the same boot and ticks.
func TestStartIdentityHasNoBTimeInput(t *testing.T) {
	const ticks = 98765
	first := startIdentity("boot-id", ticks)
	for i := 0; i < 100; i++ {
		if startIdentity("boot-id", ticks) != first {
			t.Fatal("identity changed without a boot or tick change")
		}
	}
}
