package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestStableLayoutAndLocalDirectoryOwnership(t *testing.T) {
	base := t.TempDir()
	c := core.Config{DataDir: filepath.Join(base, "state")}
	room := core.Room{Key: `["tenant","app","project","group","chat",null]`}
	w, e := Prepare(c, room, core.Session{ID: "one"}, core.Project{})
	if e != nil {
		t.Fatal(e)
	}
	w2, e := Prepare(c, room, core.Session{ID: "two"}, core.Project{})
	if e != nil || w2.Root != w.Root {
		t.Fatal("new session changed native history root", e)
	}
	if filepath.Base(w.Root) != core.Hash(room.Key)[:32] {
		t.Fatal("TypeScript room layout changed")
	}
	if _, e = Prepare(c, room, core.Session{}, core.Project{CWD: base}); e == nil {
		t.Fatal("overlapping cwd accepted")
	}
}
func TestSnapshotBoundsAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "ok"), []byte("hello"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := Snapshot(root, filepath.Join(root, "ok"), filepath.Join(out, "copy"), 5); e != nil {
		t.Fatal(e)
	}
	if e := Snapshot(root, filepath.Join(root, "ok"), filepath.Join(out, "too-large"), 4); e == nil {
		t.Fatal("oversized export accepted")
	}
	if _, e := os.Stat(filepath.Join(out, "too-large")); !os.IsNotExist(e) {
		t.Fatal("partial oversized export left behind")
	}
	if e := Snapshot(root, filepath.Join(out, "copy"), filepath.Join(out, "escape"), 10); e == nil {
		t.Fatal("outside file accepted")
	}
	if e := os.Symlink(filepath.Join(out, "copy"), filepath.Join(root, "link")); e != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Windows symlink privilege unavailable")
		}
		t.Fatal(e)
	}
	if e := Snapshot(root, filepath.Join(root, "link"), filepath.Join(out, "link-copy"), 10); e == nil {
		t.Fatal("symlink escape accepted")
	}
}
func TestLockOwnershipAndNoImplicitStaleRemoval(t *testing.T) {
	root := t.TempDir()
	release, e := Lock(root)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Lock(root); e == nil {
		t.Fatal("duplicate service lock accepted")
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	release, e = Lock(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "gateway.lock"), []byte(`{"token":"replacement"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e = release(); e == nil {
		t.Fatal("deleted lock owned by another instance")
	}
}
func TestSeedExcludesCredentialsHooksAndLinks(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	os.WriteFile(filepath.Join(source, "README.md"), []byte("source"), 0600)
	os.WriteFile(filepath.Join(source, ".env.local"), []byte("secret"), 0600)
	os.MkdirAll(filepath.Join(source, ".git", "hooks"), 0700)
	os.WriteFile(filepath.Join(source, ".git", "hooks", "post-checkout"), []byte("do not execute"), 0600)
	os.MkdirAll(filepath.Join(source, ".claude"), 0700)
	os.WriteFile(filepath.Join(source, ".claude", "settings.json"), []byte("untrusted"), 0600)
	if e := seedTree(source, target, &seedLimit{}); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(target, "README.md")); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{".env.local", ".git", ".claude"} {
		if _, e := os.Stat(filepath.Join(target, name)); !os.IsNotExist(e) {
			t.Fatal("copied forbidden seed entry", name)
		}
	}
}
