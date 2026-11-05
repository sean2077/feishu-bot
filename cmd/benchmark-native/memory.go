package main

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type memory struct {
	Status            string          `json:"status"`
	Method            string          `json:"method"`
	Host              *int64          `json:"hostBytes"`
	Descendants       *int64          `json:"descendantBytes"`
	Count             *int            `json:"descendantCount"`
	Collection        float64         `json:"collectionMs"`
	Reason            string          `json:"reason,omitempty"`
	HostPrivate       *int64          `json:"hostPrivateBytes"`
	DescendantPrivate *int64          `json:"descendantPrivateBytes"`
	Native            *int64          `json:"nativeResidentBytes"`
	Tools             *int64          `json:"toolOrMcpResidentBytes"`
	Processes         []memoryProcess `json:"processes,omitempty"`
}
type memoryProcess struct {
	PID       int    `json:"pid"`
	ParentPID int    `json:"parentPid"`
	Kind      string `json:"kind"`
	Resident  int64  `json:"residentBytes"`
	Private   *int64 `json:"privateBytes"`
}
type processMemory struct {
	pid, ppid    int
	rss, private int64
	name         string
}

func parseStatus(data string) (processMemory, error) {
	p := processMemory{pid: -1, ppid: -1, rss: -1, private: -1}
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if f[0] == "Name:" {
			p.name = f[1]
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
	included := descendantPIDs(rows, root)
	var host, desc int64
	count := 0
	found := false
	for _, row := range rows {
		if included[row.pid] && row.rss < 0 {
			return 0, 0, 0, errors.New("process memory unavailable")
		}
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

// memorySnapshot never launches a collector process, so collection itself cannot
// enter the descendant sum. Unsupported or incomplete samples remain explicit.
func memorySnapshot(nativeName ...string) (m memory) {
	start := time.Now()
	defer func() { m.Collection = elapsed(start) }()
	rows, root, method, err := platformMemoryRows()
	m = memory{Status: "unavailable", Method: method, Reason: "snapshot-failed-or-unsupported"}
	if err != nil {
		return
	}
	host, desc, count, err := memoryFromRows(rows, root)
	if err != nil {
		return
	}
	m.Status, m.Reason = "observed", ""
	m.Host, m.Descendants, m.Count = &host, &desc, &count
	included := descendantPIDs(rows, root)
	var hostPrivate, descPrivate, nativeBytes, toolBytes int64
	privateKnown := true
	name := "claude"
	if len(nativeName) > 0 && nativeName[0] != "" {
		name = strings.TrimSuffix(strings.ToLower(filepath.Base(nativeName[0])), ".exe")
	}
	for _, row := range rows {
		if !included[row.pid] {
			continue
		}
		kind := "native-tool-or-mcp"
		if row.pid == root {
			kind = "bridge-and-fixture"
			hostPrivate = row.private
		} else {
			descPrivate += row.private
			if row.ppid == root && strings.TrimSuffix(strings.ToLower(row.name), ".exe") == name {
				kind = "native-cli"
				nativeBytes += row.rss
			} else {
				toolBytes += row.rss
			}
		}
		privateKnown = privateKnown && row.private >= 0
		item := memoryProcess{PID: row.pid, ParentPID: row.ppid, Kind: kind, Resident: row.rss}
		if row.private >= 0 {
			value := row.private
			item.Private = &value
		}
		m.Processes = append(m.Processes, item)
	}
	if privateKnown {
		m.HostPrivate, m.DescendantPrivate = &hostPrivate, &descPrivate
	}
	m.Native, m.Tools = &nativeBytes, &toolBytes
	return
}

func descendantPIDs(rows []processMemory, root int) map[int]bool {
	included := map[int]bool{root: true}
	for {
		before := len(included)
		for _, row := range rows {
			if included[row.ppid] {
				included[row.pid] = true
			}
		}
		if len(included) == before {
			return included
		}
	}
}
