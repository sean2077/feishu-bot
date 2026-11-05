package main

import "testing"

func TestProcNamespaceAndDescendantAccounting(t *testing.T) {
	p, err := parseStatus("Pid:\t1200\nPPid:\t100\nNSpid:\t1200\t7\nVmRSS:\t50 kB\n")
	if err != nil || p.pid != 1200 || p.rss != 51200 {
		t.Fatal(p, err)
	}
	rows := []processMemory{{pid: 1200, ppid: 100, rss: 50}, {pid: 1201, ppid: 1200, rss: 80}, {pid: 1202, ppid: 1201, rss: 90}, {pid: 1300, ppid: 100, rss: 999}}
	h, d, n, err := memoryFromRows(rows, 1200)
	if err != nil || h != 50 || d != 170 || n != 2 {
		t.Fatal(h, d, n, err)
	}
	if _, _, _, err = memoryFromRows(rows, 7); err == nil {
		t.Fatal("unobservable namespace PID reported zero memory")
	}
}

func TestIncompleteDescendantMemoryIsUnavailable(t *testing.T) {
	rows:=[]processMemory{{pid:1,ppid:0,rss:100},{pid:2,ppid:1,rss:-1}}
	if _,_,_,err:=memoryFromRows(rows,1);err==nil { t.Fatal("unobservable descendant reported as zero") }
}
