package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestUnsavedNativeIdentityStopsInsteadOfContinuing(t *testing.T) {
	saved := identityPersistRetry
	identityPersistRetry = []time.Duration{time.Millisecond}
	t.Cleanup(func() { identityPersistRetry = saved })
	f := newFixture(t, nil)
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_cc_failure BEFORE UPDATE OF cc ON sessions BEGIN SELECT RAISE(ABORT,'fixture identity failure'); END`); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	eventually(t, func() bool { return w.stops.Load() > 0 || f.g.getSlot(f.room().Key) == nil })
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, n := range f.tr.notices {
			if strings.Contains(n, "原生会话 ID 未能保存") {
				return true
			}
		}
		return false
	})
	if s := f.status("first"); s == "done" || s == "sent" {
		t.Fatalf("input left as handled after identity failure: %s", s)
	}
}

func TestSessionWithDeliveredInputButNoIdentityIsNotRelaunched(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, e := f.s.Session(r.Key)
	if e != nil {
		t.Fatal(e)
	}
	m := f.message("earlier", "earlier work", "member")
	if _, e = f.s.Insert(r.Key, m, "done"); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.DB.Exec("UPDATE inbox SET session=? WHERE id='earlier'", s.ID); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("next", "continue", "member"))
	eventually(t, func() bool { return f.status("next") == "cancelled" })
	if f.starts.Load() != 0 {
		t.Fatal("an unidentified session with delivered input was relaunched as a new native context")
	}
	f.send(f.message("new", "/new", "owner"))
	eventually(t, func() bool { return f.openSession(r.Key).ID != s.ID })
	f.send(f.message("after", "fresh", "member"))
	w := f.worker()
	if d := f.delivery(w); d.Message.ID != "after" {
		t.Fatalf("explicit new session did not start: %+v", d)
	}
}

func TestStopStateFailureStillReleasesTheSlot(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, "done")
	eventually(t, func() bool { return f.status("first") == "done" })
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_stop_failure BEFORE UPDATE OF status ON inbox WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'fixture stop failure'); END`); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("queued", "more", "member"))
	f.delivery(w)
	if e := f.g.stopRoom(context.Background(), f.room()); e == nil {
		t.Fatal("state failure was not reported")
	}
	if w.stops.Load() == 0 || f.g.getSlot(f.room().Key) != nil {
		t.Fatalf("stop left a dead slot: stops=%d slot=%v", w.stops.Load(), f.g.getSlot(f.room().Key) != nil)
	}
	if _, e := f.s.DB.Exec("DROP TRIGGER fixture_stop_failure"); e != nil {
		t.Fatal(e)
	}
	// The failed stop keeps dispatch away until a stop succeeds.
	if e := f.g.stopRoom(context.Background(), f.room()); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("later", "after recovery", "member"))
	nw := f.worker()
	for {
		if next := f.delivery(nw); next.Message.ID == "later" {
			break
		}
	}
}

func noticeContains(f *fixture, part string) bool {
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	for _, n := range f.tr.notices {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func TestIdentityFailureAfterFirstInputNeverRelaunchesFreshContext(t *testing.T) {
	saved := identityPersistRetry
	identityPersistRetry = []time.Duration{time.Millisecond}
	t.Cleanup(func() { identityPersistRetry = saved })
	f := newFixture(t, nil)
	f.readyMode = "input"
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_cc_failure BEFORE UPDATE OF cc ON sessions BEGIN SELECT RAISE(ABORT,'fixture identity failure'); END`); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	f.delivery(w)
	eventually(t, func() bool { return f.status("first") == "uncertain" })
	if _, e := f.s.DB.Exec("DROP TRIGGER fixture_cc_failure"); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"second", "third"} {
		f.send(f.message(id, "continue", "member"))
		eventually(t, func() bool { return f.status(id) == "cancelled" })
	}
	if f.starts.Load() != 1 {
		t.Fatalf("a fresh native context was launched for an unidentified session: starts=%d", f.starts.Load())
	}
	if !noticeContains(f, "原生会话 ID 未保存") {
		t.Fatal("refusal was not explained")
	}
}

func TestExitBeforeReadyLeavesNoContextAndAllowsRelaunch(t *testing.T) {
	f := newFixture(t, nil)
	f.readyMode = "never"
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	f.delivery(w)
	yes := true
	w.emit(core.WorkerEvent{Type: "exit", CleanupConfirmed: &yes})
	eventually(t, func() bool { return f.status("first") == "cancelled" })
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, c := range f.tr.cards {
			if strings.Contains(jsonText(c.card), "开始对话前退出") {
				return true
			}
		}
		return false
	})
	f.readyMode = ""
	f.send(f.message("second", "retry", "member"))
	nw := f.worker()
	if d := f.delivery(nw); d.Message.ID != "second" {
		t.Fatalf("relaunch after a never-started process: %+v", d)
	}
}

func TestFailedStopKeepsQueuedInputsFromBeingDelivered(t *testing.T) {
	saved := stopStateRetry
	stopStateRetry = []time.Duration{time.Millisecond}
	t.Cleanup(func() { stopStateRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("busy", "long work", "member"))
	w := f.worker()
	f.delivery(w)
	f.send(f.message("queued", "/compact", "owner"))
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_stop_failure BEFORE UPDATE OF status ON inbox WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'fixture stop failure'); END`); e != nil {
		t.Fatal(e)
	}
	if e := f.g.stopRoom(context.Background(), f.room()); e == nil {
		t.Fatal("state failure was not reported")
	}
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	f.send(f.message("after", "new input", "member"))
	time.Sleep(100 * time.Millisecond)
	if f.starts.Load() != 1 || f.status("queued") != "queued" {
		t.Fatalf("queued input delivered after a failed stop: starts=%d status=%s", f.starts.Load(), f.status("queued"))
	}
	if !noticeContains(f, "上次停止未能完成") {
		t.Fatal("blocked room did not explain itself")
	}
	if _, e := f.s.DB.Exec("DROP TRIGGER fixture_stop_failure"); e != nil {
		t.Fatal(e)
	}
	if e := f.g.stopRoom(context.Background(), f.room()); e != nil {
		t.Fatal(e)
	}
	if f.status("queued") != "cancelled" || f.status("after") != "cancelled" {
		t.Fatalf("successful stop did not cancel: %s %s", f.status("queued"), f.status("after"))
	}
	f.send(f.message("fresh", "work again", "member"))
	nw := f.worker()
	if d := f.delivery(nw); d.Message.ID != "fresh" {
		t.Fatalf("room stayed blocked after a successful stop: %+v", d)
	}
}
