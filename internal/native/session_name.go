package native

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const sessionRecordLimit = 64 << 10

// SessionRecordName returns the display name that the live Claude Code
// process recorded for nativeID. It reads only name and sessionId from
// <configDir>/sessions/<pid>.json and ignores every other field. A missing
// file, a different sessionId, an unreadable record or an empty name returns
// "" so callers can fall back to the native ID.
func SessionRecordName(configDir string, pid int, nativeID string) string {
	if pid <= 0 || nativeID == "" {
		return ""
	}
	file, err := os.Open(filepath.Join(configDir, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, sessionRecordLimit+1))
	if err != nil || len(data) > sessionRecordLimit {
		return ""
	}
	var record struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
	}
	if json.Unmarshal(data, &record) != nil || record.SessionID != nativeID {
		return ""
	}
	return strings.TrimSpace(record.Name)
}
