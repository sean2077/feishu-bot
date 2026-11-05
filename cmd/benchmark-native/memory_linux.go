package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The root PID comes from this mount's /proc/self/status, not a potentially
// different PID namespace reported by os.Getpid().
func platformMemoryRows() ([]processMemory, int, string, error) {
	const method = "/proc VmRSS; private clean+dirty+hugetlb from smaps_rollup (KiB converted to bytes)"
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, 0, method, err
	}
	root, err := parseStatus(string(data))
	if err != nil {
		return nil, 0, method, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, 0, method, err
	}
	rows := []processMemory{}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		path := filepath.Join("/proc", entry.Name())
		data, err := os.ReadFile(filepath.Join(path, "status"))
		if err != nil {
			continue
		}
		p, err := parseStatus(string(data))
		if err == nil {
			rows = append(rows, p)
		}
	}
	included := descendantPIDs(rows, root.pid)
	for i := range rows {
		p := &rows[i]
		if !included[p.pid] {
			continue
		}
		path := filepath.Join("/proc", strconv.Itoa(p.pid))
		if executable, err := os.Readlink(filepath.Join(path, "exe")); err == nil {
			p.name = filepath.Base(executable)
		}
		if data, err := os.ReadFile(filepath.Join(path, "smaps_rollup")); err == nil {
			p.private = parsePrivateRollup(string(data))
		}
	}
	return rows, root.pid, method, nil
}

func parsePrivateRollup(data string) int64 {
	var total int64
	fields := 0
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || (f[0] != "Private_Clean:" && f[0] != "Private_Dirty:" && f[0] != "Private_Hugetlb:") || f[2] != "kB" {
			continue
		}
		value, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || value < 0 {
			return -1
		}
		fields++
		total += value * 1024
	}
	if fields < 2 {
		return -1
	}
	return total
}
