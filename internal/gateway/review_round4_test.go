package gateway

import (
	"context"
	"errors"
	"github.com/sean2077/feishu-bot/internal/workspace"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestUsageWriteFailureDoesNotStickTheTurn(t *testing.T) {
	saved := finalPersistRetry
	finalPersistRetry = []time.Duration{time.Millisecond}
	t.Cleanup(func() { finalPersistRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_usage_failure BEFORE INSERT ON usage_results BEGIN SELECT RAISE(ABORT,'fixture usage failure'); END`); e != nil {
		t.Fatal(e)
	}
	f.finish(w, d, "answer despite usage failure")
	eventually(t, func() bool { return f.status("first") == "done" })
	s := f.g.getSlot(f.room().Key)
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.busy && len(s.deliveries) == 0
	})
}

func TestUnwritableInputStateDoesNotLoop(t *testing.T) {
	f := newFixture(t, nil)
	f.tr.downloadHook = func(context.Context, string, core.Resource, string, int64) error {
		return errors.New("fixture download failure")
	}
	f.send(f.message("warm", "warm up", "member"))
	w := f.worker()
	f.finish(w, f.delivery(w), "ok")
	eventually(t, func() bool { return f.status("warm") == "done" })
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_state_failure BEFORE UPDATE OF status ON inbox WHEN OLD.status='queued' AND NEW.status!='queued' BEGIN SELECT RAISE(ABORT,'fixture state failure'); END`); e != nil {
		t.Fatal(e)
	}
	m := f.message("stuck", "see file", "member")
	m.Resources = []core.Resource{{Key: "file_key", Kind: "file", Name: "a.txt"}}
	f.send(m)
	time.Sleep(500 * time.Millisecond)
	f.tr.mu.Lock()
	count := 0
	for _, n := range f.tr.notices {
		if strings.Contains(n, "未提交给模型") {
			count++
		}
	}
	downloads := f.tr.downloads
	f.tr.mu.Unlock()
	if count > 2 || downloads > 3 {
		t.Fatalf("pre-handoff failure looped: notices=%d downloads=%d", count, downloads)
	}
}

func TestRejectedProcessingReactionSettlesAndDoneStillApplies(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	if _, e := f.s.DB.Exec("INSERT INTO receipts(id,room,session,desired,at,sender) VALUES('om_receipt',?,?,'processing',?,'member')", r.Key, s.ID, time.Now().UnixMilli()); e != nil {
		t.Fatal(e)
	}
	var steps []string
	f.tr.receiptHook = func(_ context.Context, _, state, prior string) error {
		steps = append(steps, state+"<"+prior)
		if state == "processing" {
			return fixtureDeliveryFailure{}
		}
		return nil
	}
	f.g.flushReceipt("om_receipt")
	var desired, applied string
	_ = f.s.DB.QueryRow("SELECT desired,applied FROM receipts WHERE id='om_receipt'").Scan(&desired, &applied)
	if desired != "clear" || applied != "clear" {
		t.Fatalf("rejected processing reaction: desired=%q applied=%q", desired, applied)
	}
	f.g.settleReceipts(r.Key, s.ID, []core.Origin{{ID: "om_receipt"}}, true)
	eventually(t, func() bool {
		_ = f.s.DB.QueryRow("SELECT applied FROM receipts WHERE id='om_receipt'").Scan(&applied)
		return applied == "done"
	})
	if steps[len(steps)-1] != "done<clear" {
		t.Fatalf("steps=%v", steps)
	}
}

func TestStaleUnconfirmedProcessingSettlesAndDoneIsCreated(t *testing.T) {
	saved := receiptUnconfirmedTTL
	receiptUnconfirmedTTL = time.Millisecond
	t.Cleanup(func() { receiptUnconfirmedTTL = saved })
	for _, tc := range []struct{ desired, wantDesired, wantApplied, lastStep string }{
		{"done", "done", "done", "done<clear"},
		{"processing", "clear", "clear", "processing<attempt:processing"},
	} {
		f := newFixture(t, nil)
		r := f.room()
		s, _ := f.s.Session(r.Key)
		if _, e := f.s.DB.Exec("INSERT INTO receipts(id,room,session,desired,applied,at,sender) VALUES('om_receipt',?,?,?,'attempt:processing',?,'member')", r.Key, s.ID, tc.desired, time.Now().Add(-time.Hour).UnixMilli()); e != nil {
			t.Fatal(e)
		}
		var steps []string
		f.tr.receiptHook = func(_ context.Context, _, state, prior string) error {
			steps = append(steps, state+"<"+prior)
			if strings.HasPrefix(prior, "attempt:") {
				return errors.New("feishu: reaction creation remains unconfirmed")
			}
			return nil
		}
		f.g.flushReceipt("om_receipt")
		var desired, applied string
		_ = f.s.DB.QueryRow("SELECT desired,applied FROM receipts WHERE id='om_receipt'").Scan(&desired, &applied)
		if desired != tc.wantDesired || applied != tc.wantApplied || steps[len(steps)-1] != tc.lastStep {
			t.Fatalf("%s: desired=%q applied=%q steps=%v", tc.desired, desired, applied, steps)
		}
	}
}

func TestUnreadableIdentityMarkerRefusesFreshLaunch(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	session, _ := f.s.Session(r.Key)
	p, _ := f.c.Project(r.ProjectID)
	w0, e := workspace.Prepare(f.c, r, session, p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(identityMarkerPath(w0.Root, session.ID), 0700); e != nil {
		t.Fatal(e)
	}
	f.send(f.message("first", "work", "member"))
	eventually(t, func() bool { return noticeContains(f, "原生会话 ID") })
	if f.starts.Load() != 0 {
		t.Fatal("an unreadable marker was treated as absent and a fresh context started")
	}
}

func TestSweepReleasesInputsOrphanedByAFailedTurn(t *testing.T) {
	savedFinal := finalPersistRetry
	finalPersistRetry = []time.Duration{time.Millisecond}
	t.Cleanup(func() { finalPersistRetry = savedFinal })
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	for _, trigger := range []string{
		`CREATE TRIGGER fixture_reply_failure BEFORE INSERT ON outbox WHEN NEW.kind='reply' BEGIN SELECT RAISE(ABORT,'fixture'); END`,
		`CREATE TRIGGER fixture_mark_failure BEFORE UPDATE OF status ON inbox WHEN NEW.status='uncertain' BEGIN SELECT RAISE(ABORT,'fixture'); END`,
	} {
		if _, e := f.s.DB.Exec(trigger); e != nil {
			t.Fatal(e)
		}
	}
	f.finish(w, d, "lost answer")
	s := f.g.getSlot(f.room().Key)
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.deliveries) == 0
	})
	if f.status("first") != "sent" {
		t.Fatalf("fixture precondition: status=%s", f.status("first"))
	}
	for _, name := range []string{"fixture_reply_failure", "fixture_mark_failure"} {
		if _, e := f.s.DB.Exec("DROP TRIGGER " + name); e != nil {
			t.Fatal(e)
		}
	}
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	if got := f.status("first"); got != "uncertain" {
		t.Fatalf("orphaned input not released: %s", got)
	}
	if !f.g.reclaimable(s) {
		t.Fatal("process still not reclaimable after reconciliation")
	}
}

func TestMessageDuringStopRetryIsToldItWillBeCancelled(t *testing.T) {
	saved := stopStateRetry
	stopStateRetry = []time.Duration{200 * time.Millisecond}
	t.Cleanup(func() { stopStateRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("busy", "work", "member"))
	w := f.worker()
	f.delivery(w)
	if _, e := f.s.DB.Exec(`CREATE TRIGGER fixture_stop_failure BEFORE UPDATE OF status ON inbox WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'fixture'); END`); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- f.g.stopRoom(context.Background(), f.room()) }()
	eventually(t, func() bool { return f.g.stopIsBlocked(f.room().Key) })
	f.send(f.message("during", "next instruction", "member"))
	if _, e := f.s.DB.Exec("DROP TRIGGER fixture_stop_failure"); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatalf("retry should have succeeded: %v", e)
	}
	if f.status("during") != "cancelled" || !noticeContains(f, "暂存的输入会随停止一并取消") {
		t.Fatalf("held input not explained: status=%s notices=%v", f.status("during"), f.tr.notices)
	}
	var desired string
	_ = f.s.DB.QueryRow("SELECT desired FROM receipts WHERE id='during'").Scan(&desired)
	if desired == "processing" {
		t.Fatal("cancelled input kept its processing reaction")
	}
	if f.g.stopIsBlocked(f.room().Key) {
		t.Fatal("room stayed blocked after the retry succeeded")
	}
}
