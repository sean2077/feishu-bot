package main

import "testing"

func TestPrivateRollupIsNotRSSOrPSS(t *testing.T) {
	if got := parsePrivateRollup("Rss: 999 kB\nPss: 400 kB\nPrivate_Clean: 2 kB\nPrivate_Dirty: 5 kB\nPrivate_Hugetlb: 1 kB\n"); got != 8192 {
		t.Fatalf("private rollup = %d, want 8192 bytes", got)
	}
	if got := parsePrivateRollup("Rss: 999 kB\n"); got != -1 {
		t.Fatal("missing private memory reported as zero", got)
	}
}
