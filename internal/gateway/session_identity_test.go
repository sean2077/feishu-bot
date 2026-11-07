package gateway

import (
	"context"
	"github.com/sean2077/feishu-bot/internal/workspace"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

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
	stopped := make(chan error, 1)
	stopStateRetry = []time.Duration{150 * time.Millisecond}
	go func() { stopped <- f.g.stopRoom(context.Background(), f.room()) }()
	// A kick queued on the dispatch lock while the stop is still failing.
	time.Sleep(50 * time.Millisecond)
	f.g.kick(f.room())
	if e := <-stopped; e == nil {
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

func TestUnsavedIdentityIsKeptInWorkspaceAndAdoptedOnRelaunch(t *testing.T) {
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
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	var native string
	eventually(t, func() bool {
		var ok bool
		native, ok = readIdentityMarker(s.workspace.Root, d.SessionID)
		return ok
	})
	f.finish(w, d, "answer")
	eventually(t, func() bool { return f.status("first") == "done" })
	if w.stops.Load() != 0 {
		t.Fatal("a session whose ID was kept in the workspace marker was stopped")
	}
	if _, e := f.s.DB.Exec("DROP TRIGGER fixture_cc_failure"); e != nil {
		t.Fatal(e)
	}
	if e := f.g.stopRoom(context.Background(), f.room()); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("second", "continue", "member"))
	nw := f.worker()
	if next := f.delivery(nw); next.Message.ID != "second" {
		t.Fatalf("relaunch: %+v", next)
	}
	if got := f.openSession(f.room().Key); got.CCID != native {
		t.Fatalf("relaunch did not adopt the kept native ID: got %q want %q", got.CCID, native)
	}
	if _, ok := readIdentityMarker(s.workspace.Root, d.SessionID); ok {
		t.Fatal("marker was not removed after adoption")
	}
}

func TestUnkeptIdentityStopsTheRoom(t *testing.T) {
	saved := identityPersistRetry
	identityPersistRetry = []time.Duration{time.Millisecond}
	t.Cleanup(func() { identityPersistRetry = saved })
	f := newFixture(t, nil)
	r := f.room()
	session, _ := f.s.Session(r.Key)
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_cc_failure BEFORE UPDATE OF cc ON sessions BEGIN SELECT RAISE(ABORT,'fixture identity failure'); END`); e != nil {
		t.Fatal(e)
	}
	s := &slot{room: r, session: session}
	if f.g.keepNativeIdentity(s, core.NewID()) {
		t.Fatal("an ID with neither a database row nor a workspace marker was reported as kept")
	}
	s.workspace.Root = t.TempDir()
	if !f.g.keepNativeIdentity(s, core.NewID()) {
		t.Fatal("workspace marker fallback was not used")
	}
}

func TestReadyAfterStopStillKeepsTheNativeIdentity(t *testing.T) {
	f := newFixture(t, nil)
	f.readyMode = "never"
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	f.delivery(w)
	if e := f.g.stopRoom(context.Background(), f.room()); e != nil {
		t.Fatal(e)
	}
	native := core.NewID()
	w.emit(core.WorkerEvent{Type: "ready", CCID: native})
	if got := f.openSession(f.room().Key); got.CCID != native {
		t.Fatalf("a native session reported after /stop was lost: %q", got.CCID)
	}
}

func TestLostIdentityBlocksTheSessionUntilNew(t *testing.T) {
	saved := identityPersistRetry
	identityPersistRetry = []time.Duration{time.Millisecond}
	t.Cleanup(func() { identityPersistRetry = saved })
	f := newFixture(t, nil)
	f.readyMode = "input"
	r := f.room()
	session, _ := f.s.Session(r.Key)
	p, _ := f.c.Project(r.ProjectID)
	w0, e := workspace.Prepare(f.c, r, session, p)
	if e != nil {
		t.Fatal(e)
	}
	// A directory at the marker path makes the workspace fallback fail too.
	if e = os.MkdirAll(identityMarkerPath(w0.Root, session.ID), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.DB.Exec(`CREATE TRIGGER fixture_cc_failure BEFORE UPDATE OF cc ON sessions BEGIN SELECT RAISE(ABORT,'fixture identity failure'); END`); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	f.delivery(w)
	eventually(t, func() bool { return w.stops.Load() > 0 })
	if _, e = f.s.DB.Exec("DROP TRIGGER fixture_cc_failure"); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("second", "continue", "member"))
	eventually(t, func() bool { return f.status("second") == "cancelled" })
	if f.starts.Load() != 1 {
		t.Fatalf("a session whose native ID was lost was relaunched fresh: starts=%d", f.starts.Load())
	}
	f.readyMode = ""
	f.send(f.message("new", "/new", "owner"))
	eventually(t, func() bool { return f.openSession(r.Key).ID != session.ID })
	f.send(f.message("third", "fresh", "member"))
	nw := f.worker()
	if d := f.delivery(nw); d.Message.ID != "third" {
		t.Fatalf("explicit /new did not start: %+v", d)
	}
}

func TestIdentityConflictStopsWithoutMarker(t *testing.T) {
	f := newFixture(t, nil)
	f.readyMode = "never"
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	recorded := core.NewID()
	if e := f.s.SetCC(d.SessionID, recorded); e != nil {
		t.Fatal(e)
	}
	w.emit(core.WorkerEvent{Type: "ready", CCID: core.NewID()})
	eventually(t, func() bool { return w.stops.Load() > 0 })
	if got := f.openSession(f.room().Key).CCID; got != recorded {
		t.Fatalf("recorded native ID changed: %q", got)
	}
	if _, ok := readIdentityMarker(s.workspace.Root, d.SessionID); ok {
		t.Fatal("a conflicting native ID was hidden in a marker")
	}
	if !noticeContains(f, "冲突") {
		t.Fatal("conflict was not explained")
	}
}
