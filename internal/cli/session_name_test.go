package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAdoptSessionName(t *testing.T) {
	configDir := t.TempDir()
	native := "11111111-2222-3333-4444-555555555555"
	dir := filepath.Join(configDir, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	record := `{"pid":4242,"sessionId":"` + native + `","name":"example-session-82","messagingSocketPath":"ignored"}`
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(4242)+".json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	if name := adoptSessionName("", configDir, 4242, native); name != "example-session-82" {
		t.Fatalf("record name = %q", name)
	}
	if name := adoptSessionName("  explicit  name ", configDir, 4242, native); name != "explicit name" {
		t.Fatalf("explicit name = %q", name)
	}
	if name := adoptSessionName("", configDir, 4242, "different"); name != "" {
		t.Fatalf("mismatched session name = %q", name)
	}
	if name := adoptSessionName("", configDir, 0, native); name != "" {
		t.Fatalf("missing holder name = %q", name)
	}
}
