//go:build linux

package process

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// StartTime returns the kernel start time of pid combined with the boot time,
// so a reused PID after a reboot cannot compare equal. The value is an
// identity token, not a wall-clock timestamp.
func StartTime(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	// The comm field may contain spaces and parentheses; only text after the
	// final ')' is positional.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[end+1:]))
	// Field 3 is fields[0]; starttime is field 22.
	if len(fields) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	boot := bootTimeSeconds()
	if boot <= 0 {
		return ticks, true
	}
	// Linux reports starttime in clock ticks (normally 100 Hz). The scaling
	// only needs to be stable for equality checks.
	return boot*1000 + ticks*10, true
}

func bootTimeSeconds() int64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err == nil {
			return value
		}
	}
	return 0
}
