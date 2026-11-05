package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func outputTargetWaiters(g *Gateway, target string) int {
	g.outTargetLocks.mu.Lock()
	defer g.outTargetLocks.mu.Unlock()
	if lock := g.outTargetLocks.locks[target]; lock != nil { return lock.users }
	return 0
}

func TestCancelStatusWaitsForActualPriorCardUpdate(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		name := "request-still-active"
		if cancelCaller { name = "caller-cancelled-before-transport-returned" }
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			room := f.room()
			session, err := f.s.Session(room.Key)
			if err != nil { t.Fatal(err) }
			const target = "known-output-card"
			card := replyCard("回复", "completed answer", nil, 0, true, replyDetails{}, time.Second, true)
			id, err := f.g.saveOutput(f.s.DB, room, session.ID, "completed answer", true, target, "", "", card, nil, "", "", "reply", "pending")
			if err != nil { t.Fatal(err) }
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			var greyWrites atomic.Int64
			var visibleMu sync.Mutex
			var visible []string
			f.tr.updateHook = func(_ context.Context, message string, value core.Args) error {
				if message != target { t.Errorf("update changed card identity: %s", message) }
				color, _ := argsOf(value["header"])["template"].(string)
				if color == "grey" {
					greyWrites.Add(1)
					visibleMu.Lock(); visible = append(visible, "grey"); visibleMu.Unlock()
					return nil
				}
				close(entered)
				// Deliberately outlive caller cancellation, as a dispatched write
				// can still complete after its caller stops waiting.
				<-release
				visibleMu.Lock(); visible = append(visible, "old"); visibleMu.Unlock()
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct { delivered bool; err error }
			oldDone := make(chan result, 1)
			go func() { delivered, err := f.g.deliverOutput(ctx, id, room, false); oldDone <- result{delivered, err} }()
			select { case <-entered: case <-time.After(3*time.Second): t.Fatal("old update did not reach transport") }
			if cancelCaller {
				cancel()
				select {
				case result := <-oldDone:
					if result.delivered || !errors.Is(result.err, context.Canceled) { t.Fatalf("caller cancellation: %+v", result) }
				case <-time.After(3*time.Second): t.Fatal("caller did not return after cancellation")
				}
			}
			if err = f.g.cancelPendingOutputs(room, session.ID); err != nil { t.Fatal(err) }
			// Observe the queued writer through the lock barrier, not a timed
			// sleep. An early grey write is a direct counterexample to ordering.
			eventually(t, func() bool { return outputTargetWaiters(f.g, target) >= 2 || greyWrites.Load() > 0 })
			if greyWrites.Load() != 0 { t.Fatal("cancel status passed an older in-flight update") }
			releaseOnce.Do(func() { close(release) })
			if !cancelCaller {
				select {
				case result := <-oldDone:
					if result.delivered || result.err != nil { t.Fatalf("cancelled old delivery completed: %+v", result) }
				case <-time.After(3*time.Second): t.Fatal("old update did not finish")
				}
			}
			eventually(t, func() bool {
				var count int
				_ = f.s.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='status' AND target=? AND state='done'", target).Scan(&count)
				return count == 1
			})
			visibleMu.Lock()
			order := append([]string(nil), visible...)
			visibleMu.Unlock()
			if len(order) != 2 || order[0] != "old" || order[1] != "grey" { t.Fatalf("cancelled card was overwritten: %v", order) }
			if original, err := f.g.output(id); err != nil || original.State != "cancelled" { t.Fatalf("old row was reopened: %s %v", original.State, err) }
			var controls int
			if err = f.s.DB.QueryRow("SELECT COUNT(*) FROM card_controls WHERE message=?", target).Scan(&controls); err != nil || controls != 0 { t.Fatal("old callback revision was rebound after cancellation") }
			f.tr.mu.Lock(); fileCalls := len(f.tr.files); f.tr.mu.Unlock()
			if f.tr.cardCount() != 0 || fileCalls != 0 || f.starts.Load() != 0 { t.Fatal("ordering repair created a card/file/native turn") }
		})
	}
}

func TestCancelledKnownTargetIsRecheckedAfterWaitingForCardLock(t *testing.T) {
	f := newFixture(t, nil)
	room := f.room()
	session, err := f.s.Session(room.Key)
	if err != nil { t.Fatal(err) }
	const target = "blocked-output-card"
	id, err := f.g.saveOutput(f.s.DB, room, session.ID, "old answer", true, target, "", "", replyCard("回复", "old answer", nil, 0, true, replyDetails{}, time.Second, true), nil, "", "", "reply", "pending")
	if err != nil { t.Fatal(err) }
	unlock := f.g.outTargetLocks.lock(target)
	var unlockOnce sync.Once
	t.Cleanup(func() { unlockOnce.Do(unlock) })
	var oldWrites, greyWrites atomic.Int64
	f.tr.updateHook = func(_ context.Context, message string, card core.Args) error {
		if message != target { t.Errorf("wrong card target: %s", message) }
		if argsOf(card["header"])["template"] == "grey" { greyWrites.Add(1) } else { oldWrites.Add(1) }
		return nil
	}
	oldDone := make(chan error, 1)
	go func() { _, err := f.g.deliverOutput(context.Background(), id, room, false); oldDone <- err }()
	eventually(t, func() bool { return outputTargetWaiters(f.g, target) >= 2 || oldWrites.Load() > 0 })
	if oldWrites.Load() != 0 { t.Fatal("outbox update bypassed the target lock") }
	if err = f.g.cancelPendingOutputs(room, session.ID); err != nil { t.Fatal(err) }
	eventually(t, func() bool { return outputTargetWaiters(f.g, target) >= 3 || greyWrites.Load() > 0 })
	if greyWrites.Load() != 0 { t.Fatal("status update bypassed the target lock") }
	unlockOnce.Do(unlock)
	select { case err = <-oldDone: if err != nil { t.Fatal(err) }; case <-time.After(3*time.Second): t.Fatal("old queued delivery did not settle") }
	eventually(t, func() bool {
		var count int
		_ = f.s.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='status' AND state='done' AND target=?", target).Scan(&count)
		return count == 1
	})
	if oldWrites.Load() != 0 || greyWrites.Load() != 1 { t.Fatalf("state was checked before, rather than inside, the lock: old=%d grey=%d", oldWrites.Load(), greyWrites.Load()) }
	if original, err := f.g.output(id); err != nil || original.State != "cancelled" { t.Fatalf("cancelled old row was revived: %s %v", original.State, err) }
	if f.tr.cardCount() != 0 || f.starts.Load() != 0 { t.Fatal("cancellation created a new card or native worker") }
}
