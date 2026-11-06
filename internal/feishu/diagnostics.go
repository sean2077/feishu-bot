package feishu

import (
	"sync"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type websocketStats struct {
	mu sync.Mutex
	connected bool
	connections, received, acknowledgements, ackFailures, watchdogs uint64
	lastInbound int64
	ackNanos, queueNanos, maxAckNanos uint64
	queues [5]<-chan connectionJob
	peaks [5]int
	rejected [5]uint64
}

func (s *websocketStats) connect(queues [5]<-chan connectionJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = true
	s.connections++
	s.queues = queues
}

func (s *websocketStats) disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = false
	// Do not keep old queued message bodies alive after connection shutdown.
	s.queues = [5]<-chan connectionJob{}
}

func (s *websocketStats) inbound(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received++
	s.lastInbound = now.UnixMilli()
}

func (s *websocketStats) admitted(lane int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peaks[lane] = max(s.peaks[lane], len(s.queues[lane]))
}

func (s *websocketStats) refused(lane int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected[lane]++
}

func (s *websocketStats) queued(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queueNanos += uint64(max(time.Duration(0), time.Since(at)))
}

func (s *websocketStats) acknowledge(at time.Time, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := uint64(max(time.Duration(0), time.Since(at)))
	s.acknowledgements++
	s.ackNanos += n
	s.maxAckNanos = max(s.maxAckNanos, n)
	if failed {
		s.ackFailures++
	}
}

func (s *websocketStats) watchdog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchdogs++
}

func (s *websocketStats) snapshot() core.Args {
	s.mu.Lock()
	defer s.mu.Unlock()
	lanes := make([]core.Args, len(s.queues))
	for i, queue := range s.queues {
		lanes[i] = core.Args{"depth": len(queue), "capacity": 8, "peakObserved": s.peaks[i], "rejected": s.rejected[i]}
	}
	return core.Args{"connected": s.connected, "connections": s.connections, "lastInboundAt": s.lastInbound, "framesReceived": s.received, "acks": s.acknowledgements, "ackFailures": s.ackFailures, "ackMs": s.ackNanos / uint64(time.Millisecond), "maxAckMs": s.maxAckNanos / uint64(time.Millisecond), "queueWaitMs": s.queueNanos / uint64(time.Millisecond), "watchdogs": s.watchdogs, "lanes": lanes, "actionLane": 4}
}
