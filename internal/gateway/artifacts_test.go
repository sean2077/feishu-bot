package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestArtifactsRetrieveOnlyCompletedFrozenRoomFiles(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil { t.Fatal(err) }
	root := f.g.artifactRoot(r)
	if err := os.MkdirAll(root, 0700); err != nil { t.Fatal(err) }
	frozen := filepath.Join(root, core.NewID())
	if err := os.WriteFile(frozen, []byte("frozen report"), 0600); err != nil { t.Fatal(err) }
	id, err := f.g.saveOutput(f.s.DB, r, s.ID, "", true, "", "", "", nil, []core.Origin{{ID: "origin", SenderID: "member"}}, "report.txt", frozen, "file", "done")
	if err != nil { t.Fatal(err) }
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_state='done',file_target='delivered-file' WHERE id=?", id); err != nil { t.Fatal(err) }
	view, err := f.g.artifactsView(r, true)
	if err != nil || !strings.Contains(view, "report.txt") || strings.Contains(view, f.c.DataDir) {
		t.Fatal("artifact list leaked paths or omitted file", view, err)
	}
	m := f.message("get-file", "/artifacts "+id, "member")
	if err := f.g.artifactCommand(context.Background(), r, m, id); err != nil { t.Fatal(err) }
	f.tr.mu.Lock()
	files := append([]string(nil), f.tr.files...)
	f.tr.mu.Unlock()
	if len(files) != 1 || files[0] == frozen { t.Fatal("retrieval did not create a frozen delivery copy", files) }
	data, err := os.ReadFile(files[0])
	if err != nil || string(data) != "frozen report" { t.Fatal("retrieved bytes changed", string(data), err) }
	other, _ := RoomFor(f.c, "other", "")
	if err := f.g.artifactCommand(context.Background(), other, m, id); err != nil { t.Fatal(err) }
	unsafe := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(unsafe, []byte("outside"), 0600); err != nil { t.Fatal(err) }
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_path=? WHERE id=?", unsafe, id); err != nil { t.Fatal(err) }
	if err := f.g.artifactCommand(context.Background(), r, m, id); err != nil { t.Fatal(err) }
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_path=?,state='blocked' WHERE id=?", frozen, id); err != nil { t.Fatal(err) }
	if err := f.g.artifactCommand(context.Background(), r, m, id); err != nil { t.Fatal(err) }
	f.tr.mu.Lock()
	count := len(f.tr.files)
	f.tr.mu.Unlock()
	if count != 1 || f.starts.Load() != 0 { t.Fatal("artifact retrieval escaped scope or woke model", count, f.starts.Load()) }
}
