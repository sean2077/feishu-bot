package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	f := newFixture(t, func(c *core.Config) {
		c.Limits.ImageBatchMS = 5000
		c.Limits.ImageBatchMaxImages = 2
	})
	defer func() {
		if t.Failed() {
			imageBatchDiagnostics(t, f)
		}
	}()
	f.send(f.image("first", "member"))
	// Observe dispatch waiting for the open batch, then remove only its timer
	// fallback. The second message must wake this cold room on its own; a slow
	// attachment transfer must not let the 5s timer mask a missing full-batch kick.
	stopped := false
	eventually(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		wait, ok := f.g.imageTimers[f.room().Key]
		if !ok {
			return false
		}
		stopped = wait.timer.Stop()
		delete(f.g.imageTimers, f.room().Key)
		return true
	})
	if !stopped || f.starts.Load() != 0 {
		t.Fatal("collection deadline elapsed before the full-batch wakeup check")
	}
	f.send(f.image("second", "member"))
	// Closure is an admission fact, not a delivery latency assertion. Both rows
	// must already agree when Message returns, even if dispatch has marked sent.
	batchID := ""
	for _, id := range []string{"first", "second"} {
		var raw string
		if err := f.s.DB.QueryRow("SELECT payload FROM inbox WHERE room=? AND id=?", f.room().Key, id).Scan(&raw); err != nil {
			t.Fatal("cannot inspect admitted image", id, err)
		}
		var input core.Delivery
		if err := jsonUnmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		if input.ImageBatch == nil || !input.ImageBatch.Closed || input.ImageBatch.ID == "" {
			t.Fatalf("second image did not persist a closed batch for %s: %+v", id, input.ImageBatch)
		}
		if batchID != "" && input.ImageBatch.ID != batchID {
			t.Fatal("original images were not admitted to the same batch")
		}
		batchID = input.ImageBatch.ID
	}
	// Keep the cold start bounded by the fixture wait. Only the later
	// filesystem/provider phase gets a separate watchdog; it performs two
	// durable file publications.
	w := f.worker()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	var d core.Delivery
	select {
	case d = <-w.sent:
	case <-timer.C:
		t.Fatal("image batch delivery did not finish after the worker started")
	}
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

func imageBatchDiagnostics(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rows, err := f.s.DB.QueryContext(ctx, "SELECT id,status,coalesce(payload,'') FROM inbox WHERE room=? ORDER BY rowid", f.room().Key)
	if err != nil {
		t.Log("image batch storage diagnostics:", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var id, status, payload string
			if err := rows.Scan(&id, &status, &payload); err != nil {
				t.Log("image batch row:", err)
				break
			}
			t.Logf("image batch input %s: status=%s payload=%s", id, status, payload)
		}
		if err := rows.Err(); err != nil {
			t.Log("image batch rows:", err)
		}
	}
	if f.g.mu.TryLock() {
		wake, timer := f.g.wakes[f.room().Key], f.g.imageTimers[f.room().Key]
		t.Logf("image batch dispatch: wake=%+v timerDeadline=%d workerStarts=%d", wake, timer.deadline, f.starts.Load())
		f.g.mu.Unlock()
	} else {
		t.Log("image batch dispatch lock is held")
	}
	if f.tr.mu.TryLock() {
		t.Logf("image batch transport: downloads=%d notices=%q cards=%+v", f.tr.downloads, f.tr.notices, f.tr.cards)
		f.tr.mu.Unlock()
	} else {
		t.Log("image batch transport lock is held")
	}
}
func TestImageBatchOversizeRejectsAllBeforeNativeHandoff(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		// Keep the batch deadline beyond the fixture wait: the second image must
		// close the batch rather than a timeout starting the worker first.
		c.Limits.ImageBatchMS = int64((fixtureWait + 5*time.Second) / time.Millisecond)
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
	for _, file := range files {
		if strings.Contains(file.Name(), ".part-") {
			t.Fatal("rejected batch left an unfinished download", files)
		}
		// Complete, individually bounded downloads remain reusable. Rejecting
		// this batch must not delete a cache file another request may use.
		if data, err := os.ReadFile(filepath.Join(inbound, file.Name())); err != nil || string(data) != "image bytes" {
			t.Fatalf("rejected batch retained an incomplete attachment: %q %v", data, err)
		}
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
