package native

import "testing"

func TestCompatibilityRequiresNativeVersionIdentity(t *testing.T) {
	for _, tc := range []struct {
		output, actual, status string
	}{
		{"2.1.321 (Claude Code)\n", "2.1.321", "tested"},
		{"2.1.316 (Claude Code)\r\n", "2.1.316", "untested"},
		{"2.1.317 (Claude Code)\r\n", "2.1.317", "untested"},
		{"2.1.316-beta.1 (Claude Code)", "2.1.316-beta.1", "untested"},
		{"0.3.316 Claude Code SDK", "", "unavailable"},
		{"error: install 2.1.316 (Claude Code)", "", "unavailable"},
		{"2.1.316 (Claude Code)\n2.1.317 (Claude Code)", "", "unavailable"},
	} {
		got := Compatibility(tc.output)
		if got.Actual != tc.actual || got.Status != tc.status || got.Tested != "2.1.321" {
			t.Fatalf("%q: %#v", tc.output, got)
		}
	}
}
