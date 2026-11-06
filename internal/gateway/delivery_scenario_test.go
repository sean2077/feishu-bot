package gateway

import (
	"context"
	"runtime"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// These two scenarios test delivery ownership and dependencies, not a 3-second
// latency guarantee. Share one bounded budget across their real startup, input,
// persistence and receipt stages; other fixture waits keep their existing limits.
func (f *fixture) deliveryScenarioContext() context.Context {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	f.t.Cleanup(cancel)
	return ctx
}

func awaitFixtureEvent[T any](f *fixture, ctx context.Context, stage string, events <-chan T) T {
	f.t.Helper()
	select {
	case event := <-events:
		return event
	case <-ctx.Done():
		f.failDeliveryScenario(stage, ctx.Err())
		var zero T
		return zero
	}
}

func (f *fixture) awaitDeliveryState(ctx context.Context, stage string, ready func(context.Context) (bool, error)) {
	f.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := ready(ctx)
		if err != nil {
			f.failDeliveryScenario(stage, err)
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			f.failDeliveryScenario(stage, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (f *fixture) doneReceiptEvent(message string) <-chan struct{} {
	f.t.Helper()
	done := make(chan struct{}, 1)
	f.tr.mu.Lock()
	if f.tr.receiptHook != nil {
		f.tr.mu.Unlock()
		f.t.Fatal("receipt barrier would replace an existing transport hook")
	}
	f.tr.receiptHook = func(_ context.Context, id, desired, _ string) error {
		if id == message && desired == "done" {
			select {
			case done <- struct{}{}:
			default:
			}
		}
		return nil
	}
	f.tr.mu.Unlock()
	return done
}

func (f *fixture) failDeliveryScenario(stage string, cause error) {
	f.t.Helper()
	// Capture the blocked goroutines before diagnostic reads can let work move
	// to another stage. All locks below are nonblocking; DB reads share 500ms.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	stack := make([]byte, 32<<10)
	n := runtime.Stack(stack, true)
	r := f.room()
	stats := f.s.DB.Stats()
	snapshot := core.Args{"stage": stage, "factoryCalls": f.starts.Load(), "dbInUse": stats.InUse, "dbWaitCount": stats.WaitCount, "dbWaitMs": stats.WaitDuration.Milliseconds()}
	var s *slot
	if f.g.mu.TryLock() {
		s = f.g.slots[r.Key]
		snapshot["closing"], snapshot["waiting"], snapshot["profileWait"] = f.g.closing, f.g.waiting[r.Key], f.g.profileWait[r.Key]
		if wake := f.g.wakes[r.Key]; wake != nil {
			snapshot["dispatch"] = core.Args{"running": wake.running, "dirty": wake.dirty}
		} else {
			snapshot["dispatch"] = "absent"
		}
		f.g.mu.Unlock()
	} else {
		snapshot["gatewayLock"] = "busy"
	}
	if s != nil {
		if s.mu.TryLock() {
			snapshot["slot"] = core.Args{"live": s.live, "busy": s.busy, "worker": s.worker != nil, "stopUnconfirmed": s.stopUnconfirmed, "deliveries": len(s.deliveries), "requests": s.activeRequests}
			s.mu.Unlock()
		} else {
			snapshot["slot"] = "lock busy"
		}
	} else {
		snapshot["slot"] = "not observed"
	}
	if f.tr.mu.TryLock() {
		snapshot["transport"] = core.Args{"cards": len(f.tr.cards), "updates": len(f.tr.updates), "files": len(f.tr.files), "receipts": len(f.tr.receipts)}
		f.tr.mu.Unlock()
	} else {
		snapshot["transport"] = "lock busy"
	}
	queries := []struct{ name, query string }{
		{"inputs", `SELECT json_group_array(json_object('id',id,'status',status,'uuid',uuid)) FROM (SELECT id,status,uuid FROM inbox WHERE room=? ORDER BY at DESC,rowid DESC LIMIT 8)`},
		{"outputs", `SELECT json_group_array(json_object('id',id,'kind',kind,'state',state,'target',target,'fileState',file_state,'error',substr(delivery_error,1,200))) FROM (SELECT id,kind,state,target,file_state,delivery_error FROM outbox WHERE room=? ORDER BY at DESC,rowid DESC LIMIT 8)`},
		{"receipts", `SELECT json_group_array(json_object('id',id,'desired',desired,'applied',applied)) FROM (SELECT id,desired,applied FROM receipts WHERE room=? ORDER BY at DESC,rowid DESC LIMIT 8)`},
		{"audit", `SELECT json_group_array(json_object('event',event,'detail',substr(detail,1,400))) FROM (SELECT event,detail FROM audit WHERE room=? ORDER BY at DESC,rowid DESC LIMIT 8)`},
	}
	for _, q := range queries {
		var value string
		if err := f.s.DB.QueryRowContext(ctx, q.query, r.Key).Scan(&value); err != nil {
			snapshot[q.name] = err.Error()
		} else {
			snapshot[q.name] = value
		}
	}
	f.t.Logf("delivery scenario failure snapshot (observed after wait): %s", jsonText(snapshot))
	f.t.Logf("goroutines at wait failure (truncated=%t):\n%s", n == len(stack), stack[:n])
	f.t.Fatalf("delivery scenario stage %q did not complete: %v", stage, cause)
}
