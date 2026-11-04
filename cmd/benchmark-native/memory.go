package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type memory struct {
	Status      string  `json:"status"`
	Method      string  `json:"method"`
	Host        *int64  `json:"hostBytes"`
	Descendants *int64  `json:"descendantBytes"`
	Count       *int    `json:"descendantCount"`
	Collection  float64 `json:"collectionMs"`
	Reason      string  `json:"reason,omitempty"`
}
type processMemory struct {
	pid, ppid int
	rss       int64
}

func parseStatus(data string) (processMemory, error) {
	p := processMemory{pid: -1, ppid: -1, rss: -1}
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "Pid:":
			p.pid = int(v)
		case "PPid:":
			p.ppid = int(v)
		case "VmRSS:":
			p.rss = v * 1024
		}
	}
	if p.pid < 1 || p.ppid < 0 || p.rss < 0 {
		return p, errors.New("incomplete process memory")
	}
	return p, nil
}
func memoryFromRows(rows []processMemory, root int) (int64, int64, int, error) {
	included := map[int]bool{root: true}
	for {
		before := len(included)
		for _, row := range rows {
			if included[row.ppid] {
				included[row.pid] = true
			}
		}
		if len(included) == before {
			break
		}
	}
	var host, desc int64
	count := 0
	found := false
	for _, row := range rows {
		if row.pid == root {
			host = row.rss
			found = true
		} else if included[row.pid] {
			desc += row.rss
			count++
		}
	}
	if !found {
		return 0, 0, 0, errors.New("root not observed")
	}
	return host, desc, count, nil
}

// Read /proc directly: no collector child enters the sum. The root comes from
// this mount's /proc/self/status, not a potentially different PID namespace.
func memorySnapshot() (m memory) {
	start := time.Now()
	m = memory{Status: "unavailable", Method: "/proc VmRSS (KiB converted to bytes)", Reason: "snapshot-failed-or-unsupported"}
	defer func() { m.Collection = elapsed(start) }()
	if runtime.GOOS != "linux" {
		return
	}
	self, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	root, err := parseStatus(string(self))
	if err != nil {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	rows := []processMemory{}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		p, err := parseStatus(string(data))
		if err == nil {
			rows = append(rows, p)
		}
	}
	host, desc, count, err := memoryFromRows(rows, root.pid)
	if err != nil {
		return
	}
	m.Status = "observed"
	m.Reason = ""
	m.Host = &host
	m.Descendants = &desc
	m.Count = &count
	return
}
