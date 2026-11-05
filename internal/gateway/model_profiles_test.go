package gateway

import (
	"testing"

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
		if s == nil { return false }
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
		if s == nil { return false }
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopUnconfirmed
	})
	p, err := f.s.Preferences(f.room().Key, d.SessionID)
	if err != nil || p.Profile != "" || p.RequestedProfile == nil || *p.RequestedProfile != "careful" || f.starts.Load() != 1 || f.status("later") != "queued" {
		t.Fatal("unconfirmed process was replaced or profile prematurely applied", p, err)
	}
}
