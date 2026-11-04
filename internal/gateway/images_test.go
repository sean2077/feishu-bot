package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func writeFixtureFile(t *testing.T, name, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if e := os.WriteFile(p, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func (f *fixture) image(id, sender string) core.Message {
	m := f.message(id, "[image]", sender)
	m.ImageOnly = true
	m.Resources = []core.Resource{{Key: id + "-image", Kind: "image", Name: "photo.png"}}
	return m
}

func TestImageBatchKeepsEveryOriginalAndCompletesTogether(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Limits.ImageBatchMS = 200; c.Limits.ImageBatchMaxImages = 2 })
	f.send(f.image("first", "member"))
	f.send(f.image("second", "member"))
	w := f.worker()
	d := f.delivery(w)
	if len(d.BatchInputs) != 2 || len(d.BatchInputs[0].Attachments) != 1 || len(d.BatchInputs[1].Attachments) != 1 {
		t.Fatalf("batch originals/attachments missing: %+v", d)
	}
	if f.status("first") != "sent" || f.status("second") != "sent" {
		t.Fatal("original input rows missing")
	}
	f.finish(w, d, "both images processed")
	eventually(t, func() bool { return f.tr.receiptDone("first") && f.tr.receiptDone("second") })
	if f.status("first") != "done" || f.status("second") != "done" {
		t.Fatal("batch members not settled together")
	}
	select {
	case extra := <-w.sent:
		t.Fatalf("batch also sent separately: %+v", extra)
	default:
	}
}
func TestImageBatchOversizeRejectsAllBeforeNativeHandoff(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Limits.ImageBatchMS = 200
		c.Limits.ImageBatchMaxImages = 2
		c.Limits.ImageBatchBytes = 15
	})
	f.send(f.image("first", "member"))
	f.send(f.image("second", "member"))
	w := f.worker()
	eventually(t, func() bool { return f.status("first") == "cancelled" && f.status("second") == "cancelled" })
	select {
	case d := <-w.sent:
		t.Fatalf("partial image batch reached native worker: %+v", d)
	default:
	}
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	inbound := s.workspace.Inbound
	s.mu.Unlock()
	files, e := os.ReadDir(inbound)
	if e != nil {
		t.Fatal(e)
	}
	if len(files) != 0 {
		t.Fatal("rejected batch left staged files", files)
	}
}
func TestImageBatchNeverCombinesDifferentAuthors(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Limits.ImageBatchMS = 30; c.Limits.ImageBatchMaxImages = 8 })
	f.send(f.image("first", "member"))
	f.send(f.image("second", "second"))
	w := f.worker()
	a := f.delivery(w)
	b := f.delivery(w)
	if a.Message.SenderID == b.Message.SenderID || len(a.BatchInputs) != 1 || len(b.BatchInputs) != 1 {
		t.Fatal("different authors were combined", a, b)
	}
	f.finish(w, a, "one")
	f.finish(w, b, "two")
}
func TestKnownIdleCapacityReclaimsWorkerButRetainsSession(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Runtime.MaxWorkers = 1 })
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, "done")
	eventually(t, func() bool { return f.tr.receiptDone("first") })
	s, e := f.s.Session(f.room().Key)
	if e != nil {
		t.Fatal(e)
	}
	one := 1
	w.emit(core.WorkerEvent{Type: "activity", State: "idle", Background: &one})
	m := f.message("other-input", "work", "member")
	m.ChatID = "other"
	f.send(m)
	f.g.Sweep(context.Background(), time.Now().UnixMilli()+2000)
	if f.starts.Load() != 1 || w.stops.Load() != 0 {
		t.Fatal("active background worker evicted")
	}
	zero := 0
	w.emit(core.WorkerEvent{Type: "activity", State: "idle", Background: &zero})
	other := f.worker()
	_ = f.delivery(other)
	if w.stops.Load() != 1 {
		t.Fatal("idle worker not reclaimed")
	}
	current, e := f.s.Session(f.room().Key)
	if e != nil || current.ID != s.ID || current.CCID != s.CCID {
		t.Fatal("reclamation discarded session", current, e)
	}
}
