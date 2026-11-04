package main

import "testing"

func TestProcNamespaceAndDescendantAccounting(t *testing.T) {
	p, err := parseStatus("Pid:\t1200\nPPid:\t100\nNSpid:\t1200\t7\nVmRSS:\t50 kB\n")
	if err != nil || p.pid != 1200 || p.rss != 51200 {
		t.Fatal(p, err)
	}
	rows := []processMemory{{1200, 100, 50}, {1201, 1200, 80}, {1202, 1201, 90}, {1300, 100, 999}}
	h, d, n, err := memoryFromRows(rows, 1200)
	if err != nil || h != 50 || d != 170 || n != 2 {
		t.Fatal(h, d, n, err)
	}
	if _, _, _, err = memoryFromRows(rows, 7); err == nil {
		t.Fatal("unobservable namespace PID reported zero memory")
	}
}
