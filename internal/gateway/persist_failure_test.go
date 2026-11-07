package gateway

import (
	"testing"
	"time"
)

func TestFailedFinalReplyDoesNotSwallowLaterTurns(t *testing.T) {
	saved := finalPersistRetry
	finalPersistRetry = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { finalPersistRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d1 := f.delivery(w)
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_reply_failure BEFORE INSERT ON outbox WHEN NEW.kind='reply' AND NEW.text LIKE 'FAIL%' BEGIN SELECT RAISE(ABORT,'fixture persistence failure'); END`); e != nil {
		t.Fatal(e)
	}
	f.finish(w, d1, "FAIL first answer")
	eventually(t, func() bool { return f.status("first") == "uncertain" })
	s := f.g.getSlot(f.room().Key)
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.renders) == 0 && len(s.deliveries) == 0 && !s.busy
	})
	f.send(f.message("second", "next", "member"))
	d2 := f.delivery(w)
	f.finish(w, d2, "second answer")
	eventually(t, func() bool { return f.status("second") == "done" })
	var n int
	if e := f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='reply' AND text='second answer'").Scan(&n); e != nil || n != 1 {
		t.Fatalf("later reply was not persisted after a failed turn: %d %v", n, e)
	}
}

func TestTransientFinalReplyFailureIsRetried(t *testing.T) {
	saved := finalPersistRetry
	finalPersistRetry = []time.Duration{200 * time.Millisecond, 200 * time.Millisecond}
	t.Cleanup(func() { finalPersistRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d1 := f.delivery(w)
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_reply_failure BEFORE INSERT ON outbox WHEN NEW.kind='reply' BEGIN SELECT RAISE(ABORT,'fixture transient failure'); END`); e != nil {
		t.Fatal(e)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = f.s.DB.Exec("DROP TRIGGER fixture_reply_failure")
	}()
	f.finish(w, d1, "recovered answer")
	eventually(t, func() bool { return f.status("first") == "done" })
}
