package main

import (
	"sync"
	"sync/atomic"
	"time"
)

type workloadStats struct {
	Scope              string  `json:"scope"`
	Concurrent         bool    `json:"concurrent"`
	Attempts           int64   `json:"attemptedTurns"`
	Completed          int64   `json:"completedTurns"`
	PeakInFlight       int64   `json:"peakInFlightTurns"`
	ElapsedMS          float64 `json:"elapsedMs"`
	SoakElapsedMS      float64 `json:"soakElapsedMs"`
	StopReason         string  `json:"stopReason"`
	CompletedPerSecond float64 `json:"completedMockTurnsPerSecond"`
}

// Each room has one serial producer. Concurrent means overlapping outstanding
// native turns across rooms, not merely retaining several idle native workers.
// The soak deadline stops new submissions; an already submitted turn keeps the
// existing bounded 45-second delivery wait before cleanup confirms termination.
func executeWorkload(o settings, run func(int, string, bool) bool) workloadStats {
	started := time.Now()
	var attempts, completed, inFlight, peak atomic.Int64
	var failed atomic.Bool
	limit := o.MaxTurns
	if limit <= 0 {
		limit = 1000
	}
	if o.Reclaim {
		limit -= o.Sessions
	}
	limit = max(limit, 0)
	attempt := func(room int, kind string, attach bool) bool {
		if failed.Load() {
			return false
		}
		for n := attempts.Load(); ; n = attempts.Load() {
			if n >= int64(limit) {
				return false
			}
			if attempts.CompareAndSwap(n, n+1) {
				break
			}
		}
		active := inFlight.Add(1)
		for n := peak.Load(); active > n; n = peak.Load() {
			if peak.CompareAndSwap(n, active) {
				break
			}
		}
		ok := run(room, kind, attach)
		inFlight.Add(-1)
		if ok {
			completed.Add(1)
		} else {
			failed.Store(true)
		}
		return ok
	}
	eachRoom := func(fn func(int)) {
		if !o.Concurrent {
			for room := 0; room < o.Sessions; room++ {
				if failed.Load() {
					break
				}
				fn(room)
			}
			return
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for room := 0; room < o.Sessions; room++ {
			room := room
			wg.Add(1)
			go func() { defer wg.Done(); <-start; fn(room) }()
		}
		close(start)
		wg.Wait()
	}
	eachRoom(func(room int) {
		if !attempt(room, "cold-native", o.Attachments) {
			return
		}
		for warm := 0; warm < o.WarmTurns; warm++ {
			if !attempt(room, "warm-native", false) {
				return
			}
		}
	})
	soakMS := float64(0)
	reason := "planned_turns"
	if !failed.Load() && o.SoakSeconds > 0 && o.Sessions > 0 {
		soakStart := time.Now()
		deadline := soakStart.Add(time.Duration(o.SoakSeconds) * time.Second)
		if o.Concurrent {
			eachRoom(func(room int) {
				for time.Now().Before(deadline) && !failed.Load() {
					if !attempt(room, "soak-native", false) {
						return
					}
				}
			})
		} else {
			for room := 0; time.Now().Before(deadline) && !failed.Load(); room = (room + 1) % o.Sessions {
				if !attempt(room, "soak-native", false) {
					break
				}
			}
		}
		soakMS = elapsed(soakStart)
		reason = "duration"
	}
	if failed.Load() {
		reason = "turn_failed"
	} else if attempts.Load() >= int64(limit) && o.SoakSeconds > 0 {
		reason = "turn_limit"
	}
	result := workloadStats{Scope: "initial_and_soak_before_optional_reclaim", Concurrent: o.Concurrent, Attempts: attempts.Load(), Completed: completed.Load(), PeakInFlight: peak.Load(), ElapsedMS: elapsed(started), SoakElapsedMS: soakMS, StopReason: reason}
	if result.ElapsedMS > 0 {
		result.CompletedPerSecond = float64(result.Completed) * 1000 / result.ElapsedMS
	}
	return result
}
