package gateway

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// The process is already gone, but its ready and exit callbacks have not run.
// This is the actual boundary provided by native.Stop, not a delayed Stop.
type queuedIdentityWorker struct {
	*fakeWorker
	waiting, release, done chan struct{}
	once                   sync.Once
}

func (w *queuedIdentityWorker) Stop(context.Context) error {
	w.stops.Add(1)
	return nil
}
func (w *queuedIdentityWorker) WaitEvents(ctx context.Context) error {
	w.once.Do(func() { close(w.waiting) })
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestStopAndCloseDrainLateNativeIdentityBeforeReturning(t *testing.T) {
	for _, closeService := range []bool{false, true} {
		name := "stop"
		if closeService {
			name = "close"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.readyMode = "never"
			base := f.g.factory
			waiting, release, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			native := core.NewID()
			f.g.factory = func(ctx context.Context, room core.Room, session core.Session, workspace core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
				worker, err := base(ctx, room, session, workspace, emit)
				if err != nil || session.CCID != "" {
					return worker, err
				}
				w := &queuedIdentityWorker{fakeWorker: worker.(*fakeWorker), waiting: waiting, release: release, done: drained}
				go func() {
					<-release
					emit(core.WorkerEvent{Type: "ready", CCID: native})
					emit(core.WorkerEvent{Type: "exit"})
					close(drained)
				}()
				return w, nil
			}
			f.send(f.message("first", "work", "member"))
			w := f.worker()
			first := f.delivery(w)
			finished := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go func() {
				if closeService {
					finished <- f.g.Close(ctx)
				} else {
					finished <- f.g.stopRoom(ctx, f.room())
				}
			}()
			select {
			case <-waiting:
			case err := <-finished:
				t.Fatalf("returned before pending ready was processed: %v", err)
			case <-ctx.Done():
				t.Fatal("event drain was not reached")
			}
			select {
			case err := <-finished:
				t.Fatalf("event drain did not fence completion: %v", err)
			default:
			}
			releaseOnce.Do(func() { close(release) })
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			if got := f.openSession(f.room().Key); got.CCID != native || got.ID != first.SessionID {
				t.Fatalf("late identity lost: %+v", got)
			}
			if !closeService {
				f.send(f.message("next", "continue", "member"))
				next := f.delivery(f.worker())
				if next.SessionID != first.SessionID || f.openSession(f.room().Key).CCID != native {
					t.Fatal("next input did not resume the stopped native context")
				}
			}
		})
	}
}

func TestDelayedGenerationStopCannotStopNewSession(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	f.delivery(f.worker())
	old := f.g.getSlot(f.room().Key)
	f.send(f.message("new", "/new", "owner"))
	f.send(f.message("second", "new work", "member"))
	w := f.worker()
	f.delivery(w)
	if err := f.g.stopGeneration(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if w.stops.Load() != 0 || f.status("second") != "sent" {
		t.Fatal("a stale failure stopped a newer generation")
	}
}

func restartFixtureGateway(t *testing.T, f *fixture) {
	t.Helper()
	factory := f.g.factory
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f.g = New(f.c, f.s, f.tr, factory, nil)
	if err := f.g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityWriteAndMarkerFailureRemainBlockedAfterRestart(t *testing.T) {
	saved := identityPersistRetry
	identityPersistRetry = nil
	t.Cleanup(func() { identityPersistRetry = saved })
	f := newFixture(t, nil)
	f.readyMode = "never"
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	root := s.workspace.Root
	s.mu.Unlock()
	// Missing parent makes marker creation fail without leaving an unreadable
	// final marker. Restore the directory before restarting the gateway.
	if err := os.Rename(root, root+"-saved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(root+"-saved", root) })
	if _, err := f.s.DB.Exec(`CREATE TRIGGER fail_ready_identity BEFORE UPDATE OF cc ON sessions BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	w.emit(core.WorkerEvent{Type: "ready", CCID: core.NewID()})
	eventually(t, func() bool { return f.g.getSlot(f.room().Key) == nil })
	if _, err := f.s.DB.Exec("DROP TRIGGER fail_ready_identity"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root+"-saved", root); err != nil {
		t.Fatal(err)
	}
	if pending, err := f.s.NativeIdentityPending(d.SessionID); err != nil || !pending {
		t.Fatalf("launch intent was not durable: %v %v", pending, err)
	}
	restartFixtureGateway(t, f)
	f.send(f.message("after", "continue", "member"))
	eventually(t, func() bool { return f.status("after") == "cancelled" })
	if f.starts.Load() != 1 {
		t.Fatal("restart silently created a replacement native context")
	}
	f.readyMode = ""
	f.send(f.message("new", "/new", "owner"))
	f.send(f.message("fresh", "start explicitly", "member"))
	if next := f.delivery(f.worker()); next.SessionID == d.SessionID {
		t.Fatal("explicit new did not leave the unresolved context behind")
	}
}

func TestConfirmedFactoryFailureBeforeInputCanRetry(t *testing.T) {
	f := newFixture(t, nil)
	base := f.g.factory
	var attempts atomic.Int64
	f.g.factory = func(ctx context.Context, room core.Room, session core.Session, workspace core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("fixture pre-launch failure")
		}
		return base(ctx, room, session, workspace, emit)
	}
	f.send(f.message("first", "work", "member"))
	eventually(t, func() bool { return attempts.Load() == 1 && f.g.getSlot(f.room().Key) == nil })
	f.g.kick(f.room())
	if d := f.delivery(f.worker()); d.Message.ID != "first" {
		t.Fatal("a confirmed pre-input failure made a fresh session unrecoverable")
	}
}

func TestFailedStopPauseSurvivesRestartUntilExplicitStop(t *testing.T) {
	saved := stopStateRetry
	stopStateRetry = nil
	t.Cleanup(func() { stopStateRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("busy", "work", "member"))
	w := f.worker()
	f.delivery(w)
	f.send(f.message("queued", "/compact", "owner"))
	if _, err := f.s.DB.Exec(`CREATE TRIGGER fail_stop_state BEFORE UPDATE OF status ON inbox WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.g.stopRoom(context.Background(), f.room()); err == nil {
		t.Fatal("stop fault did not reach cancellation")
	}
	if _, err := f.s.DB.Exec("DROP TRIGGER fail_stop_state"); err != nil {
		t.Fatal(err)
	}
	restartFixtureGateway(t, f)
	if !f.g.stopIsBlocked(f.room().Key) || f.status("queued") != "queued" {
		t.Fatal("restart removed the failed stop pause")
	}
	f.send(f.message("held", "wait until stop completes", "member"))
	if f.starts.Load() != 1 {
		t.Fatal("a paused input started native work after restart")
	}
	if err := f.g.stopRoom(context.Background(), f.room()); err != nil {
		t.Fatal(err)
	}
	if f.status("held") != "cancelled" || f.status("queued") != "cancelled" {
		t.Fatal("successful stop did not cancel held inputs")
	}
	f.send(f.message("fresh", "continue original session", "member"))
	if d := f.delivery(f.worker()); d.Message.ID != "fresh" {
		t.Fatal("room did not resume after explicit stop")
	}
}

func TestExitStateFailureIsReconciledWithoutReplayingInput(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Limits.InboxPerRoom = 1 })
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	native := f.openSession(f.room().Key).CCID
	if _, err := f.s.DB.Exec(`CREATE TRIGGER fail_exit_state BEFORE UPDATE OF status ON inbox WHEN NEW.status='uncertain' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	w.emit(core.WorkerEvent{Type: "exit"})
	if f.status("first") != "sent" {
		t.Fatal("fault did not leave an orphaned sent input")
	}
	if _, err := f.s.DB.Exec("DROP TRIGGER fail_exit_state"); err != nil {
		t.Fatal(err)
	}
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	if f.status("first") != "uncertain" {
		t.Fatal("exit orphan still consumed pending capacity after storage recovered")
	}
	f.send(f.message("next", "continue", "member"))
	next := f.delivery(f.worker())
	if next.Message.ID != "next" || next.SessionID != d.SessionID || f.openSession(f.room().Key).CCID != native {
		t.Fatalf("reconciliation replayed or replaced context: %+v", next)
	}
}

func TestStopPauseMarkerSurvivesDatabaseIntentFailure(t *testing.T) {
	saved := stopStateRetry
	stopStateRetry = nil
	t.Cleanup(func() { stopStateRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("busy", "work", "member"))
	f.delivery(f.worker())
	f.send(f.message("queued", "/compact", "owner"))
	for _, query := range []string{
		`CREATE TRIGGER fail_stop_intent BEFORE INSERT ON room_stop_pauses BEGIN SELECT RAISE(ABORT,'fixture'); END`,
		`CREATE TRIGGER fail_stop_state BEFORE UPDATE OF status ON inbox WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'fixture'); END`,
	} {
		if _, err := f.s.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.g.stopRoom(context.Background(), f.room()); err == nil {
		t.Fatal("cancellation fault was ignored")
	}
	if rooms, err := f.g.stopPauseMarkers(); err != nil || len(rooms) != 1 || rooms[0] != f.room().Key {
		t.Fatalf("fallback pause missing: %v %v", rooms, err)
	}
	for _, name := range []string{"fail_stop_intent", "fail_stop_state"} {
		if _, err := f.s.DB.Exec("DROP TRIGGER " + name); err != nil {
			t.Fatal(err)
		}
	}
	restartFixtureGateway(t, f)
	if !f.g.stopIsBlocked(f.room().Key) || f.starts.Load() != 1 {
		t.Fatal("fallback marker did not fence restart dispatch")
	}
	if err := f.g.stopRoom(context.Background(), f.room()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.g.stopPausePath(f.room().Key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful stop did not clear fallback marker: %v", err)
	}
	if f.status("queued") != "cancelled" {
		t.Fatal("fallback pause released without cancelling queued input")
	}
}

func TestReleasedExitCapacityWakesPersistedInputsWithoutAnotherSweep(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Runtime.MaxWorkers = 1 })
	f.send(f.message("first", "work", "member"))
	f.delivery(f.worker())
	s := f.g.getSlot(f.room().Key)
	other, ok := RoomFor(f.c, "other", "")
	if !ok {
		t.Fatal("fixture room missing")
	}
	ss, err := f.s.Session(other.Key)
	if err != nil {
		t.Fatal(err)
	}
	m := f.message("waiting", "queued work", "member")
	m.ChatID = other.ChatID
	// Persist input without scheduling it, as when a prior capacity kick has
	// already found the pool full and returned. Releasing this exited slot
	// alone must schedule the durable work; no timer or extra message helps.
	d := core.Delivery{UUID: core.NewID(), RoomKey: other.Key, SessionID: ss.ID, Message: m}
	if _, err = f.s.Insert(other.Key, m, "queued"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Queue(d); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.live = false
	s.mu.Unlock()
	f.g.releaseExited(s)
	if got := f.delivery(f.worker()); got.Message.ID != "waiting" {
		t.Fatalf("released capacity did not wake saved work: %+v", got)
	}
}
