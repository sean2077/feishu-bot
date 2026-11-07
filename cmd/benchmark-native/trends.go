package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type progressLog struct {
	mu   sync.Mutex
	file *os.File
	err  error
}

func newProgressLog(dir string) (*progressLog, error) {
	f, err := os.OpenFile(filepath.Join(dir, "observations.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &progressLog{file: f}, nil
}
func (l *progressLog) record(kind string, value any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = json.NewEncoder(l.file).Encode(core.Args{"kind": kind, "at": time.Now().UnixMilli(), "value": value})
	}
}
func (l *progressLog) close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.file.Close()
	if l.err != nil {
		return l.err
	}
	return err
}

type maintenanceObservation struct {
	Runs           int64  `json:"runs"`
	LastDurationMS *int64 `json:"lastDurationMs"`
	LastCandidates *int64 `json:"lastCandidates"`
	LastChanged    *int64 `json:"lastChanged"`
	LastError      string `json:"lastError,omitempty"`
}
type maintenanceObservations struct {
	mu    sync.Mutex
	value maintenanceObservation
}

func (m *maintenanceObservations) record(event string, args core.Args) {
	if event != "store_maintenance" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value.Runs++
	value := func(key string) *int64 {
		n, ok := args[key].(int64)
		if !ok {
			return nil
		}
		return &n
	}
	m.value.LastDurationMS = value("durationMs")
	m.value.LastCandidates = value("candidates")
	m.value.LastChanged = value("changed")
	m.value.LastError, _ = args["error"].(string)
}
func (m *maintenanceObservations) snapshot() maintenanceObservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.value
}

type trendPoint struct {
	ElapsedMS     float64                `json:"elapsedMs"`
	DBBytes       *int64                 `json:"databaseBytes"`
	WALBytes      *int64                 `json:"walBytes"`
	Goroutines    int                    `json:"goroutines"`
	HeapBytes     uint64                 `json:"goHeapBytes"`
	HeapInUse     uint64                 `json:"goHeapInUseBytes"`
	DataBytes     *int64                 `json:"dataDirectoryBytes"`
	DataFiles     *int64                 `json:"dataDirectoryFiles"`
	DataAtMS      float64                `json:"dataDirectoryObservedMs"`
	ActiveWorkers int64                  `json:"activeWorkers"`
	InFlight      int64                  `json:"inFlightTurns"`
	Memory        memory                 `json:"memory"`
	Maintenance   maintenanceObservation `json:"maintenance"`
	Errors        []string               `json:"errors,omitempty"`
}
type trendSeries struct {
	IntervalMS            int          `json:"intervalMs"`
	MaintenanceIntervalMS int          `json:"maintenanceIntervalMs"`
	Initial               *trendPoint  `json:"initial"`
	Points                []trendPoint `json:"points"`
	Dropped               int          `json:"droppedEarlierPoints"`
	CollectionMS          float64      `json:"collectionMs"`
}
type trendSampler struct {
	cancel context.CancelFunc
	done   <-chan trendSeries
}

const maxTrendPoints = 2000

func trendFileBytes(path string, missingIsZero bool) (*int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) && missingIsZero {
		zero := int64(0)
		return &zero, nil
	}
	if err != nil {
		return nil, err
	}
	n := info.Size()
	return &n, nil
}

// Trend points are collected during work and appended to JSONL immediately.
// The in-memory recent window is bounded independently of the workload length;
// the initial point and dropped-point count make truncation explicit.
func startTrendSampler(program, dbPath string, interval, maintenanceInterval time.Duration, workers, inFlight func() int64, observed *maintenanceObservations, sweep func(context.Context, int64), progress *progressLog) *trendSampler {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan trendSeries, 1)
	go func() {
		started := time.Now()
		result := trendSeries{IntervalMS: int(interval / time.Millisecond), MaintenanceIntervalMS: int(maintenanceInterval / time.Millisecond), Points: []trendPoint{}}
		next := 0
		var diskBytes, diskFiles *int64
		var diskAt float64
		var nextDisk time.Time
		capture := func() {
			begin := time.Now()
			point := trendPoint{ElapsedMS: elapsed(started), Goroutines: runtime.NumGoroutine(), ActiveWorkers: workers(), InFlight: inFlight(), Memory: memorySnapshot(program), Maintenance: observed.snapshot()}
			var heap runtime.MemStats
			runtime.ReadMemStats(&heap)
			point.HeapBytes, point.HeapInUse = heap.HeapAlloc, heap.HeapInuse
			if !begin.Before(nextDisk) {
				var bytes, files int64
				err := filepath.WalkDir(filepath.Dir(dbPath), func(path string, entry fs.DirEntry, err error) error {
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					if entry.Type().IsRegular() {
						info, err := entry.Info()
						if os.IsNotExist(err) {
							return nil
						}
						if err != nil {
							return err
						}
						bytes += info.Size()
						files++
					}
					return nil
				})
				if err != nil {
					diskBytes, diskFiles = nil, nil
					point.Errors = append(point.Errors, "data-directory-stat")
				} else {
					diskBytes, diskFiles = &bytes, &files
				}
				diskAt, nextDisk = elapsed(started), begin.Add(10*time.Second)
			}
			point.DataBytes, point.DataFiles, point.DataAtMS = diskBytes, diskFiles, diskAt
			// Phase/peak observations retain the detailed PID tree. Keeping only
			// aggregates here avoids multiplying that tree by 2000 trend points.
			point.Memory.Processes = nil
			read := func(path, label string, missingIsZero bool) *int64 {
				n, err := trendFileBytes(path, missingIsZero)
				if err != nil {
					point.Errors = append(point.Errors, label)
				}
				return n
			}
			point.DBBytes = read(dbPath, "database-stat", false)
			point.WALBytes = read(dbPath+"-wal", "wal-stat", true)
			if result.Initial == nil {
				copy := point
				result.Initial = &copy
			}
			if len(result.Points) < maxTrendPoints {
				result.Points = append(result.Points, point)
			} else {
				result.Points[next] = point
				next = (next + 1) % maxTrendPoints
				result.Dropped++
			}
			progress.record("trend", point)
			result.CollectionMS += elapsed(begin)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var maintenance *time.Ticker
		var maintain <-chan time.Time
		if maintenanceInterval > 0 {
			maintenance = time.NewTicker(maintenanceInterval)
			maintain = maintenance.C
			defer maintenance.Stop()
		}
		capture()
		for {
			select {
			case <-ctx.Done():
				capture()
				if result.Dropped > 0 && next > 0 {
					ordered := append([]trendPoint(nil), result.Points[next:]...)
					result.Points = append(ordered, result.Points[:next]...)
				}
				done <- result
				return
			case <-ticker.C:
				capture()
			case at := <-maintain:
				maintenanceCtx, stop := context.WithTimeout(ctx, 2*time.Second)
				sweep(maintenanceCtx, at.UnixMilli())
				stop()
			}
		}
	}()
	return &trendSampler{cancel: cancel, done: done}
}
func (s *trendSampler) stop() trendSeries { s.cancel(); return <-s.done }
