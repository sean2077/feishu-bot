package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestChatWorkerQuotaLeavesCapacityForOtherChats(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Runtime.MaxWorkers = 3
		c.Runtime.MaxWorkersPerChat = 1
	})
	f.send(f.message("quota-a", "first topic", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.send(f.message("quota-b", "second topic", "member"))
	eventually(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		return f.g.capacityWait[f.topic("quota-b").Key].reason == "chat_limit"
	})
	if f.starts.Load() != 1 || w.stops.Load() != 0 {
		t.Fatal("chat quota evicted active work or started another topic")
	}
	other := f.message("quota-other", "other chat", "member")
	other.ChatID = "other"
	f.send(other)
	ow := f.worker()
	if next := f.delivery(ow); next.Message.ID != other.ID {
		t.Fatal(next)
	}
	f.finish(w, d, "done")
	next := f.worker()
	if got := f.delivery(next); got.Message.ID != "quota-b" {
		t.Fatal(got)
	}
	if w.stops.Load() != 1 {
		t.Fatal("idle topic was not reclaimed within its chat quota")
	}
}

func TestMemoryAdmissionWaitsButPreservesExistingInputStreams(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Runtime.MaxRSSMiB = 128 })
	var observed atomic.Int64
	observed.Store(256 << 20)
	f.g.poolMu.Lock()
	f.g.residentBytes = func(context.Context) (int64, error) { return observed.Load(), nil }
	f.g.poolMu.Unlock()
	f.send(f.message("memory-first", "wait for capacity", "member"))
	eventually(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		return f.g.capacityWait[f.room().Key].reason == "memory_limit"
	})
	if f.starts.Load() != 0 || f.status("memory-first") != "queued" {
		t.Fatal("memory limit did not preserve queued input")
	}
	observed.Store(32 << 20)
	f.g.kickWaiting()
	w := f.worker()
	f.delivery(w)
	observed.Store(512 << 20)
	f.send(f.message("memory-stream", "continue existing stream", "member"))
	if got := f.delivery(w); got.Message.ID != "memory-stream" {
		t.Fatal(got)
	}
	f.g.Sweep(context.Background(), time.Now().UnixMilli()+100000)
	if w.stops.Load() != 0 {
		t.Fatal("memory pressure stopped an active native stream")
	}
}

func TestUnavailableMemoryDoesNotPretendCapacityIsKnown(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Runtime.MaxRSSMiB = 128 })
	f.g.poolMu.Lock()
	f.g.residentBytes = func(context.Context) (int64, error) { return 0, errors.New("observation unavailable") }
	f.g.poolMu.Unlock()
	f.send(f.message("memory-unknown", "wait", "member"))
	eventually(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		return f.g.capacityWait[f.room().Key].reason == "memory_unavailable"
	})
	if f.starts.Load() != 0 {
		t.Fatal("unknown RSS admitted a new process")
	}
}

func TestReuseAwareIdleBudgetKeepsFrequentlyUsedProcessWarm(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Runtime.IdleMinMS = 100
		c.Runtime.IdleMS = 1000
	})
	f.send(f.message("reuse-first", "first", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, "done")
	s := f.g.getSlot(f.room().Key)
	eventually(t, func() bool { return f.g.idleLifetime(s) == 100 && f.g.reclaimable(s) })
	f.send(f.message("reuse-second", "second", "member"))
	d = f.delivery(w)
	f.finish(w, d, "done")
	eventually(t, func() bool { return f.g.idleLifetime(s) == 200 && f.g.reclaimable(s) })
	s.mu.Lock()
	last := s.lastActivity
	s.mu.Unlock()
	f.g.Sweep(context.Background(), last+150)
	if w.stops.Load() != 0 {
		t.Fatal("reused process ignored its longer idle budget")
	}
	f.g.Sweep(context.Background(), last+201)
	if w.stops.Load() != 1 {
		t.Fatal("expired idle process was not reclaimed")
	}
}

func TestMemoryPressureDoesNotResampleBusyWorkers(t *testing.T) {
	g := &Gateway{ctx: context.Background(), slots: map[string]*slot{}}
	g.config.Runtime.MaxRSSMiB = 1
	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("busy-%d", i)
		g.slots[key] = &slot{room: core.Room{Key: key}, live: true, busy: true}
	}
	observations := 0
	g.residentBytes = func(context.Context) (int64, error) {
		observations++
		return 2 << 20, nil
	}
	g.reclaimPressure(context.Background())
	if observations > 1 {
		t.Fatalf("sampled a complete process tree %d times without a possible release", observations)
	}
}

func TestMemoryPressureResamplesOnlyAfterConfirmedRelease(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Runtime.MaxWorkers = 3
		c.Runtime.MaxRSSMiB = 1
	})
	f.g.poolMu.Lock()
	f.g.residentBytes = func(context.Context) (int64, error) { return 0, nil }
	f.g.poolMu.Unlock()
	workers := make([]*fakeWorker, 3)
	slots := make([]*slot, 3)
	for i := range workers {
		id := fmt.Sprintf("pressure-%d", i)
		f.send(f.message(id, "work", "member"))
		workers[i] = f.worker()
		f.finish(workers[i], f.delivery(workers[i]), "done")
		slots[i] = f.g.getSlot(f.topic(id).Key)
		eventually(t, func() bool { return f.g.reclaimable(slots[i]) })
		slots[i].mu.Lock()
		slots[i].lastActivity = int64(i)
		slots[i].mu.Unlock()
	}
	workers[0].failStop.Store(true)
	t.Cleanup(func() { workers[0].failStop.Store(false) })
	var observations atomic.Int64
	f.g.poolMu.Lock()
	f.g.residentBytes = func(context.Context) (int64, error) {
		if observations.Add(1) == 1 {
			return 2 << 20, nil
		}
		return 0, nil
	}
	f.g.reclaimPressure(context.Background())
	f.g.poolMu.Unlock()
	if observations.Load() != 2 || workers[0].stops.Load() != 1 || workers[1].stops.Load() != 1 || workers[2].stops.Load() != 0 {
		t.Fatalf("unexpected observations/stops: %d, [%d %d %d]", observations.Load(), workers[0].stops.Load(), workers[1].stops.Load(), workers[2].stops.Load())
	}
	if f.g.getSlot(slots[0].room.Key) != slots[0] || f.g.getSlot(slots[1].room.Key) != nil || f.g.getSlot(slots[2].room.Key) != slots[2] {
		t.Fatal("unconfirmed or unnecessary releases changed process ownership")
	}
}

func TestCancelledMemoryReclamationDoesNotSampleOrStop(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Runtime.MaxRSSMiB = 1 })
	f.g.poolMu.Lock()
	f.g.residentBytes = func(context.Context) (int64, error) { return 0, nil }
	f.g.poolMu.Unlock()
	f.send(f.message("cancelled-pressure", "work", "member"))
	w := f.worker()
	f.finish(w, f.delivery(w), "done")
	s := f.g.getSlot(f.room().Key)
	eventually(t, func() bool { return f.g.reclaimable(s) })
	var observations atomic.Int64
	f.g.poolMu.Lock()
	f.g.residentBytes = func(context.Context) (int64, error) {
		observations.Add(1)
		return 2 << 20, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.g.reclaimPressure(ctx)
	f.g.poolMu.Unlock()
	if observations.Load() != 0 || w.stops.Load() != 0 {
		t.Fatal("cancelled maintenance sampled memory or stopped an idle worker")
	}
}
