package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestTopicWorkspacesKeepStateAndAttachmentsButUseDistinctDirectories(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "feature-a"), filepath.Join(base, "feature-b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		// A worktree has a .git file. Preparation must not replace or initialize it.
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: ../repo/.git/worktrees/fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := core.Config{TenantID: "tenant", AppID: "app", DataDir: filepath.Join(base, "state"),
		Projects: []core.Project{{ID: "project", Maintainers: []string{"owner"}, SeedDir: filepath.Join(base, "unused-seed")}},
		Groups:   []core.Group{{ChatID: "oc_topics", ProjectID: "project", SessionScope: "thread", ThreadWorkspaces: map[string]string{"om_a": a, "om_b": b}}}}
	p := c.Projects[0]
	ra, _ := c.ThreadRoomFor("oc_topics", "om_a")
	rb, _ := c.ThreadRoomFor("oc_topics", "om_b")
	wa, err := Prepare(c, ra, core.Session{}, p)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := Prepare(c, rb, core.Session{}, p)
	if err != nil {
		t.Fatal(err)
	}
	physicalA, _ := physicalDirectory(a)
	physicalB, _ := physicalDirectory(b)
	if wa.Work != physicalA || wb.Work != physicalB || wa.Root != wb.Root || wa.Home != wb.Home || wa.Inbound == wb.Inbound {
		t.Fatalf("bound workspaces do not preserve layout: a=%+v b=%+v", wa, wb)
	}
	if err := os.WriteFile(filepath.Join(wa.Work, "result.txt"), []byte("topic A"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wb.Work, "result.txt")); !os.IsNotExist(err) {
		t.Fatal("topic A wrote into topic B's directory", err)
	}
	if _, _, err := TranslateOutput(wb, filepath.Join(wa.Work, "result.txt")); err == nil {
		t.Fatal("topic B exported another workspace's file")
	}
	for _, dir := range []string{a, b} {
		data, err := os.ReadFile(filepath.Join(dir, ".git"))
		if err != nil || string(data) != "gitdir: ../repo/.git/worktrees/fixture\n" {
			t.Fatal("preparation changed the existing worktree", err)
		}
	}
	// Unbound topics still use the managed directory, and /new keeps a binding.
	p.SeedDir = ""
	unbound, _ := c.ThreadRoomFor("oc_topics", "om_unbound")
	wDefault, err := Prepare(c, unbound, core.Session{}, p)
	if err != nil || wDefault.Work != filepath.Join(wa.Root, "workspace") || wDefault.Home != wa.Home {
		t.Fatal("unbound topic changed its default layout", wDefault, err)
	}
	again, err := Prepare(c, ra, core.Session{ID: "new-session"}, p)
	if err != nil || again.Work != wa.Work || again.Inbound != wa.Inbound {
		t.Fatal("a new session changed the topic binding", err)
	}
}

func TestTopicWorkspaceRefusesMissingAndStateDirectories(t *testing.T) {
	c := core.Config{DataDir: t.TempDir()}
	r := core.Room{Key: "topic", ChatKey: "chat", Thread: "om_root"}
	otherState := filepath.Join(c.DataDir, "rooms", core.Hash("other")[:32], "workspace")
	if err := os.MkdirAll(otherState, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(c.DataDir, "missing"), file, c.DataDir, otherState} {
		r.WorkspaceCWD = path
		if _, err := Prepare(c, r, core.Session{}, core.Project{}); err == nil {
			t.Fatalf("invalid topic workspace accepted: %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "missing")); !os.IsNotExist(err) {
		t.Fatal("missing workspace was silently created")
	}
}
