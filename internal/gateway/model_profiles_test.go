package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestProfileWaitsForBoundaryPreservesSupplementsAndNativeID(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"careful": {Model: "selected-model", Effort: "high"}}
	})
	f.send(f.message("first", "begin", "member"))
	w := f.worker()
	first := f.delivery(w)
	saved, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	f.send(f.message("profile", "/model careful", "owner"))
	f.send(f.message("next", "next task", "member"))
	f.send(f.message("extra", "/btw this is supplementary", "second"))
	extra := f.delivery(w)
	if extra.Message.ID != "extra" || extra.SupplementTo != first.UUID || w.stops.Load() != 0 {
		t.Fatal("profile request interrupted native stream or admitted next task", extra)
	}
	background := 1
	w.emit(core.WorkerEvent{Type: "activity", State: "idle", Background: &background})
	w.emit(core.WorkerEvent{Type: "result", UUID: first.UUID, UUIDs: []string{first.UUID, extra.UUID}, ResultID: core.NewID(), Text: "completed", Success: true, Source: "foreground"})
	eventually(t, func() bool {
		s := f.g.getSlot(f.room().Key)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.busy && s.activeRequests == 0
	})
	unlock := f.g.dispatchLocks.lock(f.room().Key)
	err = f.g.dispatch(f.room())
	unlock()
	if err != nil || w.stops.Load() != 0 || f.status("next") != "queued" {
		t.Fatal("profile evicted background work", err, f.status("next"))
	}
	background = 0
	w.emit(core.WorkerEvent{Type: "activity", State: "idle", Background: &background})
	nextWorker := f.worker()
	next := f.delivery(nextWorker)
	actual, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	if next.Message.ID != "next" || actual.ID != saved.ID || actual.CCID != saved.CCID || w.stops.Load() != 1 {
		t.Fatal("profile did not preserve session identity / queued task", actual, saved, next)
	}
	slot := f.g.getSlot(f.room().Key)
	slot.mu.Lock()
	profile := slot.session.Profile
	slot.mu.Unlock()
	if profile != "careful" {
		t.Fatal("new generation did not receive selected profile", profile)
	}
	other := f.message("other-room", "ordinary task", "member")
	other.ChatID = "other"
	f.send(other)
	otherWorker := f.worker()
	_ = f.delivery(otherWorker)
	otherRoom, _ := RoomFor(f.c, "other", "")
	otherSlot := f.g.getSlot(otherRoom.Key)
	otherSlot.mu.Lock()
	otherProfile := otherSlot.session.Profile
	otherSlot.mu.Unlock()
	if otherProfile != "" {
		t.Fatal("profile leaked to another chat")
	}
}

func TestProfileSelectionDuringColdStartupDoesNotFenceFirstInput(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"careful": {Model: "selected-model"}}
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	f.g.factory = func(ctx context.Context, _ core.Room, _ core.Session, _ core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		first.Do(func() { close(entered); select { case <-release: case <-ctx.Done(): } })
		if ctx.Err() != nil { return nil, ctx.Err() }
		w := &fakeWorker{emit: emit, sent: make(chan core.Delivery, 100), responses: make(chan core.Args, 100)}
		f.starts.Add(1)
		// Real native initialization need not report idle/CCID before input.
		f.started <- w
		return w, nil
	}
	f.send(f.message("starting-input", "work", "member"))
	select { case <-entered: case <-time.After(3*time.Second): t.Fatal("factory did not reach startup barrier") }
	f.send(f.message("profile-at-startup", "/model careful", "owner"))
	close(release)
	w := f.worker()
	d := f.delivery(w)
	if d.Message.ID != "starting-input" || w.stops.Load() != 0 { t.Fatal("startup profile selection blocked or killed its first input", d) }
	nativeID := core.NewID()
	w.emit(core.WorkerEvent{Type: "ready", CCID: nativeID})
	zero := 0
	w.emit(core.WorkerEvent{Type: "activity", State: "running", Background: &zero})
	f.send(f.message("after-profile", "next", "member"))
	f.finish(w, d, "finished first input")
	w.emit(core.WorkerEvent{Type: "activity", State: "idle", Background: &zero})
	nextWorker := f.worker()
	next := f.delivery(nextWorker)
	current, err := f.s.Session(f.room().Key)
	if err != nil || next.Message.ID != "after-profile" || current.CCID != nativeID || current.ID != d.SessionID || w.stops.Load() != 1 {
		t.Fatal("startup profile did not switch at the later safe boundary", current, next, err)
	}
}

func TestProfileSwitchKeepsUncertainOutputWithoutReplay(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"careful": {Model: "selected-model"}}
	})
	r := f.room()
	f.send(f.message("initial", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, "normal final reply")
	eventually(t, func() bool { return f.tr.receiptDone("initial") })
	before, _ := f.s.Session(r.Key)
	id, err := f.g.saveOutput(f.s.DB, r, d.SessionID, "unknown previously saved output", true, "", "", "", nil, nil, "", "", "reply", "uncertain")
	if err != nil { t.Fatal(err) }
	f.tr.mu.Lock()
	cards := len(f.tr.cards)
	f.tr.mu.Unlock()
	f.send(f.message("profile", "/profile careful", "owner"))
	f.send(f.message("following", "continue", "member"))
	nextWorker := f.worker()
	next := f.delivery(nextWorker)
	after, _ := f.s.Session(r.Key)
	output, err := f.g.output(id)
	f.tr.mu.Lock()
	afterCards := len(f.tr.cards)
	f.tr.mu.Unlock()
	if err != nil || output.State != "uncertain" || output.Target != "" || after.ID != before.ID || after.CCID != before.CCID || next.Message.ID != "following" || afterCards != cards {
		t.Fatal("profile change reset identity or replayed saved uncertain output", output, before, after, err)
	}
}

func TestProfileReadOnlyAndUnauthorizedSelectionDoNotWakeModel(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"careful": {Model: "selected-model"}}
	})
	f.send(f.message("view", "/model", "member"))
	f.send(f.message("denied", "/model careful", "member"))
	var count int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || f.starts.Load() != 0 {
		t.Fatal("read-only / unauthorized profile command started a session")
	}
}

func TestProfileSwitchKeepsCapacityWhenStopIsUnconfirmed(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"careful": {Model: "selected-model"}}
	})
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.failStop.Store(true)
	defer w.failStop.Store(false)
	f.finish(w, d, "complete")
	f.send(f.message("profile", "/model careful", "owner"))
	f.send(f.message("later", "next", "member"))
	eventually(t, func() bool {
		s := f.g.getSlot(f.room().Key)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopUnconfirmed
	})
	p, err := f.s.Preferences(f.room().Key, d.SessionID)
	if err != nil || p.Profile != "" || p.RequestedProfile == nil || *p.RequestedProfile != "careful" || f.starts.Load() != 1 || f.status("later") != "queued" {
		t.Fatal("unconfirmed process was replaced or profile prematurely applied", p, err)
	}
}
