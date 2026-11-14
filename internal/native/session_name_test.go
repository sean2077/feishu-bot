package native

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeSessionRecord(t *testing.T, configDir string, pid int, body string) {
	t.Helper()
	dir := filepath.Join(configDir, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRecordName(t *testing.T) {
	configDir := t.TempDir()
	native := "11111111-2222-3333-4444-555555555555"
	writeSessionRecord(t, configDir, 4242, `{"pid":4242,"sessionId":"`+native+`","name":"example-session-82","messagingSocketPath":"ignored"}`)
	if name := SessionRecordName(configDir, 4242, native); name != "example-session-82" {
		t.Fatalf("name = %q", name)
	}
	if name := SessionRecordName(configDir, 4242, "different"); name != "" {
		t.Fatalf("mismatched session ID name = %q", name)
	}
	if name := SessionRecordName(configDir, 0, native); name != "" {
		t.Fatalf("missing PID name = %q", name)
	}
	if name := SessionRecordName(configDir, 9999, native); name != "" {
		t.Fatalf("missing record name = %q", name)
	}
}

func TestSessionRecordNameRejectsInvalidRecords(t *testing.T) {
	configDir := t.TempDir()
	native := "11111111-2222-3333-4444-555555555555"
	writeSessionRecord(t, configDir, 1, `{`)
	if name := SessionRecordName(configDir, 1, native); name != "" {
		t.Fatalf("invalid JSON name = %q", name)
	}
	writeSessionRecord(t, configDir, 2, `{"sessionId":"`+native+`"}`)
	if name := SessionRecordName(configDir, 2, native); name != "" {
		t.Fatalf("empty name = %q", name)
	}
	writeSessionRecord(t, configDir, 3, `{"sessionId":"`+native+`","name":"`+strings.Repeat("x", sessionRecordLimit+1)+`"}`)
	if name := SessionRecordName(configDir, 3, native); name != "" {
		t.Fatalf("oversized record name = %q", name)
	}
}
