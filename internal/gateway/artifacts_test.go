package gateway

import (
	"context"
	"errors"
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
	if err != nil {
		t.Fatal(err)
	}
	root := f.g.artifactRoot(r)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	frozen := filepath.Join(root, core.NewID())
	if err := os.WriteFile(frozen, []byte("frozen report"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := f.g.saveOutput(f.s.DB, r, s.ID, "", true, "", "", "", nil, []core.Origin{{ID: "origin", SenderID: "member"}}, "report.txt", frozen, "file", "done")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_state='done',file_target='delivered-file' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	view, err := f.g.artifactsView(r, true)
	if err != nil || !strings.Contains(view, "report.txt") || strings.Contains(view, f.c.DataDir) {
		t.Fatal("artifact list leaked paths or omitted file", view, err)
	}
	m := f.message("get-file", "/artifacts "+id, "member")
	if err := f.g.artifactCommand(context.Background(), r, m, id); err != nil {
		t.Fatal(err)
	}
	f.tr.mu.Lock()
	files := append([]string(nil), f.tr.files...)
	f.tr.mu.Unlock()
	if len(files) != 1 || files[0] == frozen {
		t.Fatal("retrieval did not create a frozen delivery copy", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || string(data) != "frozen report" {
		t.Fatal("retrieved bytes changed", string(data), err)
	}
	other, _ := RoomFor(f.c, "other", "")
	if err := f.g.artifactCommand(context.Background(), other, m, id); err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(unsafe, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_path=? WHERE id=?", unsafe, id); err != nil {
		t.Fatal(err)
	}
	if err := f.g.artifactCommand(context.Background(), r, m, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_path=?,state='blocked' WHERE id=?", frozen, id); err != nil {
		t.Fatal(err)
	}
	if err := f.g.artifactCommand(context.Background(), r, m, id); err != nil {
		t.Fatal(err)
	}
	f.tr.mu.Lock()
	count := len(f.tr.files)
	f.tr.mu.Unlock()
	if count != 1 || f.starts.Load() != 0 {
		t.Fatal("artifact retrieval escaped scope or woke model", count, f.starts.Load())
	}
}

func TestFailedArtifactRetrievalDoesNotBlockNativeReplyDone(t *testing.T) {
	f := newFixture(t, nil)
	ctx := f.deliveryScenarioContext()
	done := f.doneReceiptEvent("native-task")
	r := f.room()
	f.send(f.message("native-task", "current task", "member"))
	w := awaitFixtureEvent(f, ctx, "worker factory and startup events", f.started)
	d := awaitFixtureEvent(f, ctx, "input delivered to worker", w.sent)
	root := f.g.artifactRoot(r)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	frozen := filepath.Join(root, core.NewID())
	if err := os.WriteFile(frozen, []byte("previous completed artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := f.g.saveOutput(f.s.DB, r, d.SessionID, "", true, "", "", "", nil, nil, "previous.txt", frozen, "file", "done")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("UPDATE outbox SET file_state='done',file_target='already-delivered' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	f.tr.mu.Lock()
	f.tr.fileHook = func(context.Context, string) (string, error) {
		return "", errors.New("fixture lost file acknowledgement")
	}
	f.tr.mu.Unlock()
	if err := f.g.command(ctx, r, f.message("retrieve", "/artifacts "+id, "member")); err == nil {
		t.Fatal("fixture did not fail the explicit artifact re-delivery")
	}
	var pending int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE room=? AND session=? AND kind='artifact' AND state='uncertain'", r.Key, d.SessionID).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("artifact failure did not retain its independent delivery record", pending, err)
	}
	f.tr.mu.Lock()
	f.tr.fileHook = nil
	f.tr.mu.Unlock()
	if f.tr.receiptDone("native-task") {
		t.Fatal("native task received Done before its reply completed")
	}
	f.finish(w, d, "Current native reply requires no file.")
	awaitFixtureEvent(f, ctx, "native reply Done with independent uncertain artifact", done)
	if err := f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE room=? AND kind='artifact' AND state='uncertain'", r.Key).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("native completion replayed or discarded unrelated artifact retrieval", pending, err)
	}
}
