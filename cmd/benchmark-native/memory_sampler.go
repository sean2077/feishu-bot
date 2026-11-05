package main

import (
	"context"
	"time"
)

type memorySamples struct {
	IntervalMS   int     `json:"intervalMs"`
	Observed     int     `json:"observedSamples"`
	Unavailable  int     `json:"unavailableSamples"`
	CollectionMS float64 `json:"collectionMs"`
	Peak         memory  `json:"sampledPeakMemory"`
}

func (s *memorySamples) observe(m memory) {
	s.CollectionMS += m.Collection
	if m.Status != "observed" || m.Host == nil || m.Descendants == nil {
		s.Unavailable++
		return
	}
	s.Observed++
	if s.Peak.Host == nil || s.Peak.Descendants == nil || *m.Host+*m.Descendants > *s.Peak.Host+*s.Peak.Descendants {
		s.Peak = m
	}
}

type memorySampler struct {
	cancel context.CancelFunc
	done   <-chan memorySamples
}

// Sampling belongs only to this measurement command. It never changes gateway
// liveness or touches the model. A 100 ms sampled peak is not an absolute peak;
// collection overhead is included explicitly in the report.
func startMemorySampler(program string) *memorySampler {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan memorySamples, 1)
	go func() {
		result := memorySamples{IntervalMS: 100, Peak: memory{Status: "unavailable", Reason: "no-observable-sample"}}
		timer := time.NewTicker(100 * time.Millisecond)
		defer timer.Stop()
		result.observe(memorySnapshot(program))
		for {
			select {
			case <-ctx.Done():
				done <- result
				return
			case <-timer.C:
				result.observe(memorySnapshot(program))
			}
		}
	}()
	return &memorySampler{cancel: cancel, done: done}
}
func (s *memorySampler) stop() memorySamples { s.cancel(); return <-s.done }
