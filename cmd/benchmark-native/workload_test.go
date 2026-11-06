package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestConcurrentWorkloadOverlapsNativeTurnWaits(t *testing.T) {
	entered := make(chan int, 3)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan workloadStats, 1)
	go func() {
		done <- executeWorkload(settings{Sessions: 3, Concurrent: true, MaxTurns: 3}, func(room int, _ string, _ bool) bool { entered <- room; <-release; return true })
	}()
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		select {
		case room := <-entered:
			seen[room] = true
		case <-time.After(3 * time.Second):
			t.Fatal("rooms were retained sequentially instead of overlapping their turn waits")
		}
	}
	unblock()
	result := <-done
	if len(seen) != 3 || result.PeakInFlight != 3 || result.Attempts != 3 || result.Completed != 3 {
		t.Fatal(result, seen)
	}
}

func TestSequentialDefaultsAndSoakLimits(t *testing.T) {
	var order []string
	result := executeWorkload(settings{Sessions: 2, WarmTurns: 1, MaxTurns: 4}, func(room int, kind string, _ bool) bool {
		order = append(order, string(rune('0'+room))+":"+kind)
		return true
	})
	if !reflect.DeepEqual(order, []string{"0:cold-native", "0:warm-native", "1:cold-native", "1:warm-native"}) || result.PeakInFlight != 1 || result.StopReason != "planned_turns" {
		t.Fatal(order, result)
	}
	var calls atomic.Int64
	result = executeWorkload(settings{Sessions: 2, Concurrent: true, SoakSeconds: 10, MaxTurns: 5}, func(int, string, bool) bool { calls.Add(1); return true })
	if calls.Load() != 5 || result.Attempts != 5 || result.Completed != 5 || result.StopReason != "turn_limit" {
		t.Fatal("sustained loop exceeded its turn budget", calls.Load(), result)
	}
	if workloadFits(settings{Scenario: "matrix", WarmTurns: 2, MaxTurns: 3}) {
		t.Fatal("matrix warmup ignored its four-session minimum")
	}
}

func TestTrendObservationsAreWrittenBeforeSamplerStops(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gateway.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log, err := newProgressLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var observed maintenanceObservations
	sampler := startTrendSampler("", dbPath, 10*time.Millisecond, 15*time.Millisecond, func() int64 { return 1 }, func() int64 { return 0 }, &observed, func(ctx context.Context, now int64) {
		r, e := st.Maintain(ctx, now, 1000)
		a := core.Args{"durationMs": r.DurationMS, "candidates": r.Candidates, "changed": r.Changed}
		if e != nil {
			a["error"] = e.Error()
		}
		observed.record("store_maintenance", a)
		log.record("maintenance", a)
	}, log)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			sampler.stop()
		}
		log.close()
	})
	deadline := time.Now().Add(3 * time.Second)
	live := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(dir, "observations.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), `"kind":"trend"`) >= 3 && strings.Contains(string(data), `"kind":"maintenance"`) {
			live = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !live {
		t.Fatal("observations only became available after the workload ended")
	}
	series := sampler.stop()
	stopped = true
	if len(series.Points) < 3 || series.Initial == nil || series.Points[len(series.Points)-1].Maintenance.Runs == 0 {
		t.Fatal(series)
	}
	for _, p := range series.Points {
		if p.DBBytes == nil || *p.DBBytes <= 0 || p.WALBytes == nil || p.Goroutines < 1 || p.ActiveWorkers != 1 {
			t.Fatal("incomplete live resource observation", p)
		}
	}
}

func TestRealNativeConcurrentSoakKeepsSessionsAndDeliveryEvidence(t *testing.T) {
	path := os.Getenv("CLAUDE_NATIVE_PATH")
	if path == "" {
		t.Skip("set CLAUDE_NATIVE_PATH to run concurrent native fixture turns")
	}
	t.Setenv("FEISHU_BASELINE_FIXTURE_TOKEN", fixtureToken)
	s := runSample(1, settings{Scenario: "warm", Sessions: 2, Concurrent: true, SoakSeconds: 1, MaxTurns: 8, SampleMS: 100, IdleMS: 100, Claude: path})
	if s.Status != "passed" || !s.Cleanup || !s.Stable || s.Workers != 2 || s.Workload.PeakInFlight < 2 || len(s.Turns) < 2 || len(s.Turns) > 8 || len(s.Trends.Points) < 2 {
		t.Fatalf("concurrent native contract failed: %+v", s)
	}
	for _, turn := range s.Turns {
		if !turn.Success || turn.ElapsedMS <= 0 || turn.Sequence < 1 || turn.First == nil || turn.Delivery == nil {
			t.Fatal("missing per-turn delivery evidence", turn)
		}
	}
}

func TestTrendMissingMeasurementsRemainUnknown(t *testing.T) {
	var observations maintenanceObservations
	initial := observations.snapshot()
	if initial.Runs != 0 || initial.LastDurationMS != nil || initial.LastCandidates != nil || initial.LastChanged != nil {
		t.Fatal("unobserved maintenance became a zero-cost measurement", initial)
	}
	path := filepath.Join(t.TempDir(), "absent.sqlite")
	if n, err := trendFileBytes(path, false); err == nil || n != nil {
		t.Fatal("missing database became zero bytes", n, err)
	}
	if n, err := trendFileBytes(path+"-wal", true); err != nil || n == nil || *n != 0 {
		t.Fatal("absent WAL should have zero allocated file bytes", n, err)
	}
	observations.record("store_maintenance", core.Args{"durationMs": int64(0), "candidates": int64(0), "changed": int64(0)})
	measured := observations.snapshot()
	if measured.Runs != 1 || measured.LastDurationMS == nil || *measured.LastDurationMS != 0 {
		t.Fatal("observed zero-cost sweep lost its measurement", measured)
	}
}
