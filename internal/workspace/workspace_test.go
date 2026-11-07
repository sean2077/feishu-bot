package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

func TestCancelledPreparationDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PrepareContext(ctx, core.Config{DataDir: root}, core.Room{Key: "room"}, core.Session{}, core.Project{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("cancelled startup created private state", err)
	}
}

func TestSeedRetryCannotFollowDestinationLink(t *testing.T) {
	source, target, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "document.txt"), []byte("seed content"), 0600); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(outside, "protected.txt")
	if err := os.WriteFile(protected, []byte("keep original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(protected, filepath.Join(target, "document.txt")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Windows symlink privilege unavailable")
		}
		t.Fatal(err)
	}
	if err := seedTree(source, target, &seedLimit{}); err == nil {
		t.Fatal("seed retry followed an existing destination link")
	}
	data, err := os.ReadFile(protected)
	if err != nil || string(data) != "keep original" {
		t.Fatal("seed overwrote outside its workspace", string(data), err)
	}
}

func TestPreparationRejectsRedirectedPrivateDirectories(t *testing.T) {
	for _, name := range []string{"workspace", "home", "inbound"} {
		t.Run(name, func(t *testing.T) {
			c := core.Config{DataDir: t.TempDir()}
			room := core.Room{Key: "room"}
			w, err := Prepare(c, room, core.Session{}, core.Project{})
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(w.Root, name)
			if err = os.Remove(directory); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(t.TempDir(), directory); err != nil {
				if runtime.GOOS == "windows" {
					t.Skip("Windows symlink privilege unavailable")
				}
				t.Fatal(err)
			}
			if _, err = Prepare(c, room, core.Session{}, core.Project{}); err == nil {
				t.Fatal("redirected private storage admitted", name)
			}
		})
	}
}

func TestLockReleaseIsIdempotentAcrossSubsequentOwner(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	next, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next()
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		go func() { errs <- release() }()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal("release was not idempotent", err)
		}
	}
	if unexpected, err := Lock(dir); err == nil {
		_ = unexpected()
		t.Fatal("previous owner released the new owner's lock")
	}
}

func TestSeededWorkspaceInitializesGitOnceAndKeepsEdits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git not installed")
	}
	source := t.TempDir()
	file := filepath.Join(source, "README.md")
	if err := os.WriteFile(file, []byte("initial source"), 0600); err != nil {
		t.Fatal(err)
	}
	c := core.Config{DataDir: t.TempDir()}
	room := core.Room{Key: "seeded-room"}
	project := core.Project{SeedDir: source}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w, err := PrepareContext(ctx, c, room, core.Session{ID: "first"}, project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.Work, ".git", "HEAD")); err != nil {
		t.Fatal("Git baseline was not initialized", err)
	}
	if err := os.WriteFile(filepath.Join(w.Work, "README.md"), []byte("conversation edits"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := PrepareContext(ctx, c, room, core.Session{ID: "second"}, project)
	if err != nil || next.Work != w.Work {
		t.Fatal(next, err)
	}
	data, err := os.ReadFile(filepath.Join(w.Work, "README.md"))
	if err != nil || string(data) != "conversation edits" {
		t.Fatal("new session reseeded a durable workspace", string(data), err)
	}
}

func TestTopicsShareWorkspaceButNotAttachments(t *testing.T) {
	base := t.TempDir()
	c := core.Config{DataDir: filepath.Join(base, "state")}
	chat := core.Room{Key: `["tenant","app","project","group","chat",null]`}
	chat.ChatKey = chat.Key
	a, b := chat, chat
	a.Key, a.Thread = `["tenant","app","project","group","chat",null,"om_a"]`, "om_a"
	b.Key, b.Thread = `["tenant","app","project","group","chat",null,"om_b"]`, "om_b"
	wc, e := Prepare(c, chat, core.Session{}, core.Project{})
	if e != nil {
		t.Fatal(e)
	}
	wa, e := Prepare(c, a, core.Session{}, core.Project{})
	if e != nil {
		t.Fatal(e)
	}
	wb, e := Prepare(c, b, core.Session{}, core.Project{})
	if e != nil {
		t.Fatal(e)
	}
	if wa.Root != wc.Root || wb.Root != wc.Root || wa.Work != wc.Work || wa.Home != wc.Home {
		t.Fatal("topics of one chat must share its workspace and native HOME")
	}
	if wc.Inbound != filepath.Join(wc.Root, "inbound") || wa.Inbound == wb.Inbound || wa.Inbound == wc.Inbound {
		t.Fatalf("attachment directories: chat=%s a=%s b=%s", wc.Inbound, wa.Inbound, wb.Inbound)
	}
	upload := filepath.Join(wa.Inbound, "upload.txt")
	if err := os.WriteFile(upload, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := TranslateOutput(wa, upload); err != nil {
		t.Fatalf("own topic attachment rejected: %v", err)
	}
	if _, _, err := TranslateOutput(wb, upload); err == nil {
		t.Fatal("another topic's attachment was accepted for output")
	}
	if _, _, err := TranslateOutput(wc, upload); err == nil {
		t.Fatal("the chat room accepted a topic attachment for output")
	}
}

func TestSharedPreparationWaitHonorsCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	unlock, err := lockPrepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err = lockPrepare(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	unlock()
	again, err := lockPrepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestLegacyTopicAttachmentsMoveOutOfTheChatInbound(t *testing.T) {
	base := t.TempDir()
	c := core.Config{DataDir: filepath.Join(base, "state")}
	chat := core.Room{Key: `["tenant","app","project","group","chat",null]`}
	chat.ChatKey = chat.Key
	topic := chat
	topic.Key, topic.Thread = `["tenant","app","project","group","chat",null,"om_a"]`, "om_a"
	wc, e := Prepare(c, chat, core.Session{}, core.Project{})
	if e != nil {
		t.Fatal(e)
	}
	id := filepath.Base(topic.AttachmentDir(wc.Root))
	legacy := filepath.Join(wc.Root, "inbound", "topics", id)
	if e = os.MkdirAll(legacy, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(legacy, "upload.txt"), []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	wt, e := Prepare(c, topic, core.Session{}, core.Project{})
	if e != nil {
		t.Fatal(e)
	}
	if data, e := os.ReadFile(filepath.Join(wt.Inbound, "upload.txt")); e != nil || string(data) != "x" {
		t.Fatalf("legacy topic upload not moved: %q %v", data, e)
	}
	if _, e := os.Stat(filepath.Join(wc.Root, "inbound", "topics")); !os.IsNotExist(e) {
		t.Fatal("legacy topics directory left inside the chat inbound")
	}
}

func TestPartialLegacyMigrationIsMerged(t *testing.T) {
	// Direct helper calls use the same canonical root as PrepareContext.
	// TempDir may contain /var aliases or Windows short/case-normalized names.
	root, err := physicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "inbound", "topics", "topic-a")
	target := filepath.Join(root, "topic-inbound", "topic-a")
	for _, p := range []string{legacy, target} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(legacy, "old.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("y"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{legacy, target} {
		if err := os.WriteFile(filepath.Join(dir, "same.txt"), []byte("z"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateTopicAttachments(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "old.txt")); err != nil || string(data) != "x" {
		t.Fatalf("legacy upload not merged: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "inbound", "topics")); !os.IsNotExist(err) {
		t.Fatal("legacy topics directory left inside the chat inbound")
	}
}
