// benchmark-native runs production gateway/native code against a local model
// and in-memory Feishu transport. It never reads deployment configuration.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/gateway"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/store"
)

type settings struct {
	Samples   int    `json:"samples"`
	WarmTurns int    `json:"warmTurns"`
	IdleMS    int    `json:"idleMs"`
	Claude    string `json:"-"`
}
type turn struct {
	Kind     string    `json:"kind"`
	Success  bool      `json:"success"`
	First    *float64  `json:"firstOutputMs"`
	Result   *float64  `json:"nativeResultMs"`
	Delivery *float64  `json:"mockDeliveryMs"`
	Usage    core.Args `json:"usage"`
}
type sample struct {
	Index         int      `json:"index"`
	Status        string   `json:"status"`
	Failures      []string `json:"failures"`
	ServiceInit   float64  `json:"serviceInitMs"`
	Workers       int      `json:"workersStarted"`
	Stable        bool     `json:"sessionStable"`
	Turns         []turn   `json:"turns"`
	Memory        memory   `json:"idleMemory"`
	Stop          float64  `json:"stopMs"`
	Cleanup       bool     `json:"cleanupConfirmed"`
	ModelRequests int64    `json:"modelRequests"`
}
type observation struct {
	id                  string
	started             time.Time
	first, result, done *float64
	success             bool
	usage               core.Args
}

func elapsed(t time.Time) float64    { return float64(time.Since(t).Microseconds()) / 1000 }
func timestamp(t time.Time) *float64 { v := elapsed(t); return &v }
func usage(input core.Args) core.Args {
	out := core.Args{}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		out[k] = nil
		if v, ok := input[k].(float64); ok && v >= 0 {
			out[k] = v
		}
	}
	return out
}

type transport struct{ onReceipt func(string, string) }

func (*transport) Send(context.Context, core.Room, string, string, string) (string, error) {
	return core.NewID(), nil
}
func (*transport) Card(context.Context, core.Room, core.Args, string, string) (string, error) {
	return core.NewID(), nil
}
func (*transport) Update(context.Context, string, core.Args) error { return nil }
func (*transport) Download(context.Context, string, core.Resource, string, int64) error {
	return errors.New("fixture does not download")
}
func (*transport) Fetch(context.Context, string, core.Room) (*core.Message, error) { return nil, nil }
func (*transport) History(context.Context, core.Room, core.HistoryQuery) (core.HistoryPage, error) {
	return core.HistoryPage{Messages: []core.Message{}}, nil
}
func (t *transport) Receipt(_ context.Context, id, state, _ string) error {
	t.onReceipt(id, state)
	return nil
}
func (*transport) File(context.Context, core.Room, string, string) (string, error) {
	return "", errors.New("fixture does not upload")
}

func modelFixture() (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			http.Error(w, "fixture endpoint only", 404)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, (8<<20)+1))
		if err != nil || len(data) > 8<<20 {
			http.Error(w, "fixture input limit", 400)
			return
		}
		var b core.Args
		if json.Unmarshal(data, &b) != nil {
			http.Error(w, "fixture JSON", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "count_tokens") {
			_, _ = io.WriteString(w, `{"input_tokens":500}`)
			return
		}
		n := calls.Add(1)
		u := core.Args{"input_tokens": 500, "output_tokens": 30, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
		message := core.Args{"id": fmt.Sprintf("msg_fixture_%d", n), "type": "message", "role": "assistant", "model": "fixture-model", "content": []core.Args{{"type": "text", "text": "BENCHMARK_OK"}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": u}
		if stream, _ := b["stream"].(bool); !stream {
			_ = json.NewEncoder(w).Encode(message)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, a core.Args) {
			a["type"] = kind
			value, _ := json.Marshal(a)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, value)
		}
		message["content"] = []core.Args{}
		message["stop_reason"] = nil
		message["usage"] = core.Args{"input_tokens": 500, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
		event("message_start", core.Args{"message": message})
		event("content_block_start", core.Args{"index": 0, "content_block": core.Args{"type": "text", "text": ""}})
		event("content_block_delta", core.Args{"index": 0, "delta": core.Args{"type": "text_delta", "text": "BENCHMARK_OK"}})
		event("content_block_stop", core.Args{"index": 0})
		event("message_delta", core.Args{"delta": core.Args{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": core.Args{"output_tokens": 30}})
		event("message_stop", core.Args{})
	}))
	return server, &calls
}

func runSample(index int, o settings) (result sample) {
	result = sample{Index: index, Status: "failed", Stable: true, Failures: []string{}, Turns: []turn{}}
	dir, err := os.MkdirTemp("", "feishu-go-baseline-")
	if err != nil {
		result.Failures = append(result.Failures, "temporary-directory")
		return
	}
	server, calls := modelFixture()
	defer server.Close()
	var st *store.Store
	var g *gateway.Gateway
	var mu sync.Mutex
	var current *observation
	defer func() {
		start := time.Now()
		if g != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := g.Close(ctx)
			cancel()
			result.Cleanup = err == nil
			if err != nil {
				result.Failures = append(result.Failures, "shutdown-unconfirmed")
			}
		} else {
			result.Cleanup = true
		}
		result.Stop = elapsed(start)
		if st != nil {
			if err := st.Close(); err != nil {
				result.Failures = append(result.Failures, "store-close")
			}
		}
		result.ModelRequests = calls.Load()
		if result.Cleanup {
			_ = os.RemoveAll(dir)
		}
		if len(result.Failures) == 0 {
			result.Status = "passed"
		}
	}()
	start := time.Now()
	c := config.Defaults()
	c.TenantID = "tenant_fixture"
	c.AppID = "cli_fixture"
	c.AppSecretEnv = "FEISHU_BASELINE_UNUSED_APP_SECRET"
	c.DataDir = dir
	c.Runtime.MaxWorkers = 1
	c.Runtime.IdleMS = 0
	c.Runtime.ClaudeExecutable = o.Claude
	c.Model.BaseURL = server.URL
	c.Model.AllowHTTP = true
	c.Model.TokenEnv = "FEISHU_BASELINE_FIXTURE_TOKEN"
	c.Model.Model = "fixture-model"
	c.Model.SmallModel = "fixture-model"
	c.Projects = []core.Project{{ID: "baseline", Maintainers: []string{"alice"}, PermissionMode: "default", NativeConfig: "isolated"}}
	c.Groups = []core.Group{{ChatID: "fixture-chat", ProjectID: "baseline", Members: []string{"alice"}}}
	c.Limits.UpdateMS = 250
	st, err = store.Open(filepath.Join(dir, "gateway.sqlite"))
	if err != nil {
		result.Failures = append(result.Failures, "store-open")
		return
	}
	tr := &transport{onReceipt: func(id, state string) {
		mu.Lock()
		defer mu.Unlock()
		if current != nil && current.id == id && state == "done" && current.done == nil {
			current.done = timestamp(current.started)
		}
	}}
	factory := native.NewFactory(c, func(string, core.Args) {})
	var workers atomic.Int64
	var nativeFailure atomic.Bool
	wrapped := func(ctx context.Context, room core.Room, session core.Session, w core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		workers.Add(1)
		return factory(ctx, room, session, w, func(e core.WorkerEvent) {
			mu.Lock()
			if current != nil {
				if (e.Type == "text" || e.Type == "thinking") && e.Text != "" && current.first == nil {
					current.first = timestamp(current.started)
				}
				if e.Type == "result" {
					current.result = timestamp(current.started)
					current.success = e.Success
					current.usage = usage(e.Usage)
				}
			}
			if e.Type == "error" {
				nativeFailure.Store(true)
			}
			mu.Unlock()
			emit(e)
		})
	}
	g = gateway.New(c, st, tr, wrapped, func(string, core.Args) {})
	if err = g.Recover(context.Background()); err != nil {
		result.Failures = append(result.Failures, "recovery")
		return
	}
	result.ServiceInit = elapsed(start)
	var previous core.Session
	for i := 0; i <= o.WarmTurns; i++ {
		input := core.Message{ID: core.NewID(), ChatID: "fixture-chat", ChatType: "group", SenderID: "alice", Text: "Reply BENCHMARK_OK without using tools.", Mentioned: true, Time: time.Now().UnixMilli()}
		mu.Lock()
		current = &observation{id: input.ID, started: time.Now(), usage: usage(nil)}
		mu.Unlock()
		if err = g.Message(context.Background(), input); err != nil {
			result.Failures = append(result.Failures, "inbound")
			break
		}
		deadline := time.NewTimer(20 * time.Second)
		ticker := time.NewTicker(10 * time.Millisecond)
		waiting := true
		for waiting {
			select {
			case <-deadline.C:
				waiting = false
			case <-ticker.C:
				mu.Lock()
				done := current.done != nil
				mu.Unlock()
				if done || nativeFailure.Load() {
					waiting = false
				}
			}
		}
		deadline.Stop()
		ticker.Stop()
		mu.Lock()
		observed := *current
		current = nil
		mu.Unlock()
		kind := "cold-native"
		if i > 0 {
			kind = "warm-native"
		}
		result.Turns = append(result.Turns, turn{Kind: kind, Success: observed.success && observed.done != nil, First: observed.first, Result: observed.result, Delivery: observed.done, Usage: observed.usage})
		if observed.done == nil || observed.first == nil || !observed.success {
			result.Failures = append(result.Failures, "turn-not-delivered")
			break
		}
		room, ok := gateway.Route(c, input)
		if !ok {
			result.Failures = append(result.Failures, "route")
			break
		}
		session, err := st.Session(room.Key)
		if err != nil || session.CCID == "" || (previous.ID != "" && (session.ID != previous.ID || session.CCID != previous.CCID)) {
			result.Stable = false
			result.Failures = append(result.Failures, "session-changed")
			break
		}
		previous = session
	}
	result.Workers = int(workers.Load())
	if result.Workers != 1 {
		result.Failures = append(result.Failures, "unexpected-worker-count")
	}
	timer := time.NewTimer(time.Duration(o.IdleMS) * time.Millisecond)
	<-timer.C
	result.Memory = memorySnapshot()
	return
}
func distribution(v []float64) core.Args {
	sort.Float64s(v)
	if len(v) == 0 {
		return core.Args{"count": 0, "min": nil, "median": nil, "max": nil}
	}
	median := v[len(v)/2]
	if len(v)%2 == 0 {
		median = (v[len(v)/2-1] + median) / 2
	}
	return core.Args{"count": len(v), "min": v[0], "median": median, "max": v[len(v)-1]}
}
func main() {
	var o settings
	flag.IntVar(&o.Samples, "samples", 3, "independent native sessions (1..10)")
	flag.IntVar(&o.WarmTurns, "warm-turns", 2, "warm continuations per session (1..5)")
	flag.IntVar(&o.IdleMS, "idle-ms", 1000, "idle observation window (100..10000)")
	flag.StringVar(&o.Claude, "claude", os.Getenv("CLAUDE_NATIVE_PATH"), "native Claude executable")
	flag.Parse()
	if flag.NArg() != 0 || o.Samples < 1 || o.Samples > 10 || o.WarmTurns < 1 || o.WarmTurns > 5 || o.IdleMS < 100 || o.IdleMS > 10000 {
		fmt.Fprintln(os.Stderr, "invalid benchmark arguments")
		os.Exit(1)
	}
	if o.Claude == "" {
		o.Claude = "claude"
	}
	if err := os.Setenv("FEISHU_BASELINE_FIXTURE_TOKEN", "local-fixture-not-a-real-credential"); err != nil {
		os.Exit(1)
	}
	samples := []sample{}
	var cold, warm, stops []float64
	passed := 0
	for i := 1; i <= o.Samples; i++ {
		s := runSample(i, o)
		samples = append(samples, s)
		if s.Status == "passed" {
			passed++
		}
		if s.Cleanup {
			stops = append(stops, s.Stop)
		}
		for _, t := range s.Turns {
			if t.Success && t.First != nil {
				if t.Kind == "cold-native" {
					cold = append(cold, *t.First)
				} else {
					warm = append(warm, *t.First)
				}
			}
		}
	}
	report := core.Args{"schema": 1, "fixture": "deterministic-text-v1", "settings": o, "environment": core.Args{"platform": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version(), "logicalCpus": runtime.NumCPU()}, "scope": core.Args{"model": "Local deterministic HTTP fixture; tokens are synthetic.", "transport": "In-memory Feishu substitute; no live acceptance.", "memory": "Host includes benchmark, Gateway, SQLite and fixture server. RSS sums can double-count shared pages; unavailable is not zero.", "cold": "Fresh native session, not cold OS caches.", "liveServiceAccepted": false}, "samples": samples, "summary": core.Args{"attemptedSamples": o.Samples, "passedSamples": passed, "failedSamples": o.Samples - passed, "coldFirstOutputMs": distribution(cold), "warmFirstOutputMs": distribution(warm), "stopMs": distribution(stops)}}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if enc.Encode(report) != nil || passed != o.Samples {
		os.Exit(1)
	}
}
