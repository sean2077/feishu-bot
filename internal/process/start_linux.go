//go:build linux

package process

import (
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
)

// StartTime returns a stable identity for pid. It combines the kernel
// starttime ticks with the boot ID, never the wall-clock boot time: NTP, VM or
// WSL clock corrections can change btime and must not make a live holder look
// like PID reuse. The value is an identity token, not a timestamp.
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
	bootID := ""
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = strings.TrimSpace(string(data))
	}
	return startIdentity(bootID, ticks), true
}

// startIdentity combines a boot identity with raw starttime ticks. It is a
// deterministic identity token; wall-clock changes are deliberately excluded.
// Without a boot ID the raw ticks are still stable within the current boot.
func startIdentity(bootID string, ticks int64) int64 {
	if bootID == "" {
		return ticks
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(bootID))
	return int64(hash.Sum64()) ^ ticks
}
