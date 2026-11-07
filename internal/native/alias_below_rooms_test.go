package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDriveAliasBelowRoomsTreeIsDenied maps a drive letter straight onto
// another room. Its parent chain never passes through the rooms tree, so only
// final-path resolution can see where it leads.
func TestDriveAliasBelowRoomsTreeIsDenied(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("subst drive aliases")
	}
	w, work, other := windowsScopeFixture(t, "inherit")
	_ = work
	if err := os.WriteFile(filepath.Join(other, "secret.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	letter := ""
	for c := 'Z'; c >= 'M'; c-- {
		if _, err := os.Stat(string(c) + `:\`); os.IsNotExist(err) {
			letter = string(c) + ":"
			break
		}
	}
	if letter == "" {
		t.Skip("no free drive letter")
	}
	if out, err := exec.Command("subst", letter, filepath.Dir(other)).CombinedOutput(); err != nil {
		t.Skipf("subst unavailable: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("subst", letter, "/D").Run() })
	for tool, input := range map[string]args{
		"Grep": {"path": letter + `\inbound`},
		"Glob": {"pattern": letter + `\inbound\*`},
		"Read": {"file_path": letter + `\inbound\secret.txt`},
	} {
		if !hookDenied(t, w, tool, input, "") {
			t.Fatalf("%s through a drive alias of another room was allowed: %v", tool, input)
		}
	}
	if !hookDenied(t, w, "Grep", args{"path": letter + `\`}, "") {
		t.Fatal("a drive alias of another room directory was allowed")
	}
}
