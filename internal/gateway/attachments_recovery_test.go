package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

func TestLongAttachmentNamesRemainDisplayMetadata(t *testing.T) {
	f := newFixture(t, nil)
	name := strings.Repeat("报", 80) + ".pdf"
	m := f.message("long-name", "read the attached report", "member")
	m.Resources = []core.Resource{{Key: "report-key", Kind: "file", Name: name}}
	f.send(m)
	w := f.worker()
	d := f.delivery(w)
	if len(d.Attachments) != 1 || d.Attachments[0].Name != name {
		t.Fatalf("attachment display name changed: %+v", d.Attachments)
	}
	path := d.Attachments[0].Path
	if len(filepath.Base(path)) > 100 || filepath.Ext(path) != ".pdf" {
		t.Fatalf("unbounded disk name or missing type extension: %q", path)
	}
	result, err := f.g.request(f.g.getSlot(f.room().Key), core.WorkerEvent{Kind: "attachment", Input: core.Args{"messageId": m.ID, "key": "report-key"}})
	if err != nil || result["path"] != path || result["name"] != name {
		t.Fatalf("fetch did not reuse the incoming attachment with its display name: %v %v", result, err)
	}
	f.tr.mu.Lock()
	downloads := f.tr.downloads
	f.tr.mu.Unlock()
	if downloads != 1 {
		t.Fatalf("incoming and on-demand access downloaded the same file %d times", downloads)
	}
}

func queueAttachmentForRecovery(t *testing.T, f *fixture, id string) (core.Delivery, core.Workspace) {
	t.Helper()
	room := f.room()
	session, err := f.s.Session(room.Key)
	if err != nil {
		t.Fatal(err)
	}
	message := f.message(id, "recover this attachment", "member")
	message.Resources = []core.Resource{{Key: "file-key", Kind: "file", Name: "report.txt"}}
	d := core.Delivery{UUID: core.NewID(), RoomKey: room.Key, SessionID: session.ID, Message: message}
	if _, err = f.s.Insert(room.Key, message, "received"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Queue(d); err != nil {
		t.Fatal(err)
	}
	project, _ := f.c.Project(room.ProjectID)
	ws, err := workspace.Prepare(f.c, room, session, project)
	if err != nil {
		t.Fatal(err)
	}
	return d, ws
}

func TestQueuedAttachmentRecoveryIgnoresIncompleteLegacyAndPrivateFiles(t *testing.T) {
	f := newFixture(t, nil)
	d, ws := queueAttachmentForRecovery(t, f, "interrupted-input")
	// An old process could die after O_EXCL created this name but before the
	// queued -> sent transaction. Neither old nor private data is a cache hit.
	legacy := filepath.Join(ws.Inbound, core.Hash(d.Message.ID)[:16]+"-0-report.txt")
	private := filepath.Join(ws.Inbound, attachmentDiskName(d.Message.ID, d.Message.Resources[0])+".part-interrupted")
	for _, path := range []string{legacy, private} {
		if err := os.WriteFile(path, []byte("truncated"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.delivery(f.worker())
	if got.UUID != d.UUID || len(got.Attachments) != 1 || got.Attachments[0].Path == legacy {
		t.Fatalf("queued input was lost or reused an old partial file: %+v", got)
	}
	data, err := os.ReadFile(got.Attachments[0].Path)
	if err != nil || string(data) != "image bytes" {
		t.Fatalf("recovered download: %q %v", data, err)
	}
}

func TestQueuedAttachmentRecoveryReusesPublishedFile(t *testing.T) {
	f := newFixture(t, nil)
	d, ws := queueAttachmentForRecovery(t, f, "published-before-crash")
	path, err := f.g.downloadAttachment(context.Background(), ws, d.Message.ID, d.Message.Resources[0])
	if err != nil {
		t.Fatal(err)
	}
	// The final file exists, but this input is still queued as it would be if
	// the process stopped immediately before its native handoff transaction.
	f.tr.mu.Lock()
	f.tr.downloadHook = func(context.Context, string, core.Resource, string, int64) error {
		return errors.New("a published attachment must not be downloaded again")
	}
	f.tr.mu.Unlock()
	if err = f.g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.delivery(f.worker())
	if got.UUID != d.UUID || len(got.Attachments) != 1 || got.Attachments[0].Path != path {
		t.Fatalf("published input did not recover: %+v", got)
	}
}

func TestAttachmentPublicationWaitsForTheCompleteDownload(t *testing.T) {
	f := newFixture(t, nil)
	d, ws := queueAttachmentForRecovery(t, f, "publication")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.tr.downloadHook = func(_ context.Context, _ string, _ core.Resource, path string, _ int64) error {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		if _, err = file.WriteString("first "); err != nil {
			return err
		}
		close(started)
		<-release
		_, err = file.WriteString("and last")
		return err
	}
	type outcome struct {
		path string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		path, err := f.g.downloadAttachment(context.Background(), ws, d.Message.ID, d.Message.Resources[0])
		done <- outcome{path, err}
	}()
	<-started
	final := filepath.Join(ws.Inbound, attachmentDiskName(d.Message.ID, d.Message.Resources[0]))
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unfinished download acquired its published name: %v", err)
	}
	once.Do(func() { close(release) })
	result := <-done
	data, err := os.ReadFile(result.path)
	if result.err != nil || err != nil || string(data) != "first and last" {
		t.Fatalf("published bytes: %q %v %v", data, result.err, err)
	}
	entries, err := os.ReadDir(ws.Inbound)
	if err != nil || len(entries) != 1 {
		t.Fatalf("private download was not removed: %v %v", entries, err)
	}
}

func TestConcurrentAttachmentFetchesPublishOneDownload(t *testing.T) {
	f := newFixture(t, nil)
	d, ws := queueAttachmentForRecovery(t, f, "concurrent-fetch")
	const readers = 8
	start := make(chan struct{})
	results := make(chan string, readers)
	errors := make(chan error, readers)
	for range readers {
		go func() {
			<-start
			path, err := f.g.downloadAttachment(context.Background(), ws, d.Message.ID, d.Message.Resources[0])
			results <- path
			errors <- err
		}()
	}
	close(start)
	first := ""
	for range readers {
		path, err := <-results, <-errors
		if err != nil {
			t.Fatal(err)
		}
		if first != "" && path != first {
			t.Fatalf("concurrent calls returned different files: %q %q", first, path)
		}
		first = path
	}
	f.tr.mu.Lock()
	downloads := f.tr.downloads
	f.tr.mu.Unlock()
	if downloads != 1 {
		t.Fatalf("concurrent cache misses made %d downloads", downloads)
	}
}
