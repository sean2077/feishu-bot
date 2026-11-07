package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestAliasSpellingOfRoomsTreeIsDeniedByIdentity reaches the rooms tree under
// a second spelling (a substituted drive letter). Text comparison cannot see
// that it is the same directory; file identity can.
func TestAliasSpellingOfRoomsTreeIsDeniedByIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("subst drive aliases")
	}
	w, work, other := windowsScopeFixture(t, "inherit")
	base := filepath.Dir(filepath.Dir(filepath.Dir(work))) // parent of rooms
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
	if out, err := exec.Command("subst", letter, base).CombinedOutput(); err != nil {
		t.Skipf("subst unavailable: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("subst", letter, "/D").Run() })
	rel, err := filepath.Rel(base, other)
	if err != nil {
		t.Fatal(err)
	}
	alias := letter + `\` + rel
	if _, err := os.Stat(alias); err != nil {
		t.Skipf("alias not visible: %v", err)
	}
	if !hookDenied(t, w, "Grep", args{"path": alias}, "") {
		t.Fatal("alias spelling of another room's attachments was allowed")
	}
	if !hookDenied(t, w, "Grep", args{"path": letter + `\`}, "") {
		t.Fatal("alias spelling of an ancestor of the rooms tree was allowed")
	}
	aliasWork := letter + `\` + mustRel(t, base, work)
	if hookDenied(t, w, "Grep", args{"path": aliasWork}, "") {
		t.Fatal("alias spelling of the own workspace was denied")
	}
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}
