package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestFailedTopicAttachmentMigrationStopsChatPreparation(t *testing.T) {
	c := core.Config{DataDir: t.TempDir()}
	chat := core.Room{Key: "chat"}
	w, err := Prepare(c, chat, core.Session{}, core.Project{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(w.Inbound, "topics", "topic-a")
	target := filepath.Join(w.Root, core.TopicInbound, "topic-a")
	for _, dir := range []string{legacy, filepath.Dir(target)} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	upload := filepath.Join(legacy, "upload.txt")
	if err = os.WriteFile(upload, []byte("topic-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, []byte("unexpected non-directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Prepare(c, chat, core.Session{}, core.Project{}); err == nil {
		t.Fatal("chat preparation succeeded after its topic migration failed")
	}
	if data, err := os.ReadFile(upload); err != nil || string(data) != "topic-owned" {
		t.Fatalf("failed migration destroyed the original upload: %q %v", data, err)
	}
	if _, _, err = TranslateOutput(w, upload); err == nil {
		t.Fatal("a previously prepared chat workspace claimed a legacy topic upload")
	}
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err = Prepare(c, chat, core.Session{}, core.Project{}); err != nil {
		t.Fatalf("migration could not resume after its blocker was corrected: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "upload.txt")); err != nil || string(data) != "topic-owned" {
		t.Fatalf("resumed migration did not preserve the upload: %q %v", data, err)
	}
}

func TestTopicMigrationPreservesConflictingCopies(t *testing.T) {
	root, err := physicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "inbound", "topics", "topic-a", "upload.txt")
	target := filepath.Join(root, core.TopicInbound, "topic-a", "upload.txt")
	for path, contents := range map[string]string{legacy: "original complete file", target: "different or truncated"} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateTopicAttachments(context.Background(), root); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("expected an attachment conflict, got %v", err)
	}
	for path, want := range map[string]string{legacy: "original complete file", target: "different or truncated"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("migration altered a conflicting copy: %q %v", got, err)
		}
	}
}

func TestCancelledTopicMigrationLeavesItsSource(t *testing.T) {
	root, err := physicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "inbound", "topics", "topic-a")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := migrateTopicAttachments(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled migration: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("cancelled migration moved its source: %v", err)
	}
}

func TestPreparationMigratesLegacyAttachmentsThroughStateAliases(t *testing.T) {
	for _, kind := range []string{"symlink", "case"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			storage := filepath.Join(base, "Physical-State")
			if err := os.Mkdir(storage, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(base, "state-link")
			if kind == "symlink" {
				if err := os.Symlink(storage, alias); err != nil {
					if runtime.GOOS == "windows" {
						t.Skip("Windows symlink privilege unavailable")
					}
					t.Fatal(err)
				}
			} else {
				alias = filepath.Join(base, "PHYSICAL-STATE")
				original, err := os.Stat(storage)
				if err != nil {
					t.Fatal(err)
				}
				alternate, err := os.Stat(alias)
				if errors.Is(err, os.ErrNotExist) {
					t.Skip("filesystem does not provide case aliases")
				}
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(original, alternate) {
					t.Fatal("case alias does not identify the same state directory")
				}
			}
			c := core.Config{DataDir: alias}
			chat := core.Room{Key: "chat"}
			w, err := Prepare(c, chat, core.Session{}, core.Project{})
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := physicalDirectory(w.Root)
			if err != nil || w.Root != canonical {
				t.Fatalf("preparation retained a directory alias: %q, canonical %q, error %v", w.Root, canonical, err)
			}
			legacy := filepath.Join(w.Root, "inbound", "topics", "topic-a")
			target := filepath.Join(w.Root, core.TopicInbound, "topic-a")
			for path, contents := range map[string]string{
				filepath.Join(legacy, "old.txt"): "old upload",
				filepath.Join(target, "new.txt"): "already migrated upload",
			} {
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			next, err := Prepare(c, chat, core.Session{}, core.Project{})
			if err != nil || next.Root != w.Root {
				t.Fatalf("preparation through the state alias failed: %v", err)
			}
			for name, want := range map[string]string{"old.txt": "old upload", "new.txt": "already migrated upload"} {
				if got, err := os.ReadFile(filepath.Join(target, name)); err != nil || string(got) != want {
					t.Fatalf("migration did not preserve %s: %q %v", name, got, err)
				}
			}
			if _, err := os.Stat(filepath.Dir(legacy)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("legacy topic attachment directory remained: %v", err)
			}
		})
	}
}
