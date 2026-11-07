package process

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func residentRows(ctx context.Context) ([]residentProcess, int, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, 0, err
	}
	self, err := parseStatus(data)
	if err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, 0, err
	}
	var rows []residentProcess
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		p, err := parseStatus(data)
		if err != nil {
			continue
		}
		rss := int64(-1)
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "VmRSS:" && fields[2] == "kB" {
				if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil && n >= 0 && n <= (1<<63-1)/1024 {
					rss = n * 1024
				}
			}
			if len(fields) >= 2 && fields[0] == "State:" && fields[1] == "Z" {
				rss = 0
			}
		}
		rows = append(rows, residentProcess{p.pid, p.parent, rss})
	}
	return rows, self.pid, nil
}
