// benchmark-native exercises production gateway/native code against local
// deterministic fixtures. It never reads deployment configuration.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
)

type settings struct {
	Samples int `json:"samples"`
	WarmTurns int `json:"warmTurns"`
	IdleMS int `json:"idleMs"`
	Scenario string `json:"scenario"`
	Sessions int `json:"sessions"`
	TextBytes int `json:"textBytes"`
	UpdateMS int `json:"updateMs"`
	TransportMS int `json:"transportMs"`
	Attachments bool `json:"attachments"`
	Reclaim bool `json:"reclaim"`
	Claude string `json:"-"`
}
type turn struct {
	Room int `json:"room"`
	Kind string `json:"kind"`
	Success bool `json:"success"`
	Matched bool `json:"fixtureReplyMatched"`
	First *float64 `json:"firstOutputMs"`
	Result *float64 `json:"nativeResultMs"`
	Delivery *float64 `json:"mockDeliveryMs"`
	Usage core.Args `json:"usage"`
}
type sample struct {
	Index int `json:"index"`
	Scenario string `json:"scenario"`
	Status string `json:"status"`
	Failures []string `json:"failures"`
	ServiceInit float64 `json:"serviceInitMs"`
	Workers int `json:"workersStarted"`
	ActiveWorkers int64 `json:"activeWorkersBeforeStop"`
	Stable bool `json:"sessionStable"`
	Reclaimed bool `json:"reclaimConfirmed"`
	Turns []turn `json:"turns"`
	Memory memory `json:"idleMemory"`
	MemoryPhases map[string]memory `json:"memoryPhases"`
	MemorySampling memorySamples `json:"memorySampling"`
	AllocatedBytes uint64 `json:"goAllocatedBytes"`
	Allocations uint64 `json:"goAllocations"`
	Transport transportStats `json:"transport"`
	Stop float64 `json:"stopMs"`
	Cleanup bool `json:"cleanupConfirmed"`
	ModelRequests int64 `json:"modelRequests"`
}
type observation struct {
	id string
	started time.Time
	first, result, done *float64
	success, matched bool
	usage core.Args
	completed chan struct{}
	once sync.Once
}

const fixtureReply = "BENCHMARK_OK"
const fixtureToken = "local-fixture-not-a-real-credential"

func elapsed(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
func timestamp(t time.Time) *float64 { v := elapsed(t); return &v }
func usage(input core.Args) core.Args {
	out := core.Args{}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		out[k] = nil
		if v, ok := input[k].(float64); ok && v >= 0 { out[k] = v }
	}
	return out
}

// observedWorker marks intentional stops before the native event can arrive.
// It counts a release only after the real process-tree stop is confirmed.
type observedWorker struct {
	core.Worker
	stopping *atomic.Bool
	onStopped func()
	once sync.Once
}
func (w *observedWorker) Stop(ctx context.Context) error {
	w.stopping.Store(true)
	err := w.Worker.Stop(ctx)
	if err == nil { w.once.Do(w.onStopped) }
	return err
}

func runSample(index int, o settings) (result sample) {
	// Preserve the original programmatic short-fixture contract.
	if o.Scenario == "" { o.Scenario, o.Sessions = "warm", 1 }
	if o.UpdateMS <= 0 { o.UpdateMS = 50 }
	result = sample{Index: index, Scenario: o.Scenario, Status: "failed", Stable: true, Cleanup: true, Failures: []string{}, Turns: []turn{}, MemoryPhases: map[string]memory{}}
	dir, err := os.MkdirTemp("", "feishu-go-baseline-")
	if err != nil { result.Failures = append(result.Failures, "temporary-directory"); return }
	reply := benchmarkReply(o.TextBytes)
	server, calls := modelFixtureReply(reply)
	defer server.Close()
	var st *store.Store
	var g *gateway.Gateway
	var sampler *memorySampler
	var mu sync.Mutex
	byRoom := map[string]*observation{}
	byID := map[string]*observation{}
	stopFlags := []*atomic.Bool{}
	var nativeFailure atomic.Bool
	var workers, active atomic.Int64
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	tr := &transport{delay: time.Duration(o.TransportMS)*time.Millisecond, onReceipt: func(id, state string) {
		mu.Lock()
		defer mu.Unlock()
		if current := byID[id]; current != nil && state == "done" && current.done == nil {
			current.done = timestamp(current.started)
			current.once.Do(func() { close(current.completed) })
		}
	}}
	defer func() {
		if sampler!=nil { result.MemorySampling=sampler.stop() }
		if nativeFailure.Load() { result.Failures = append(result.Failures, "native-failed-or-exited-before-cleanup") }
		result.Workers = int(workers.Load())
		result.ActiveWorkers = active.Load()
		start := time.Now()
		if g != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := g.Close(ctx)
			cancel()
			result.Cleanup = err == nil
			if err != nil { result.Failures = append(result.Failures, "shutdown-unconfirmed") }
		}
		result.Stop = elapsed(start)
		result.MemoryPhases["after-stop"] = memorySnapshot(o.Claude)
		if st != nil {
			if err := st.Close(); err != nil { result.Failures = append(result.Failures, "store-close") }
		}
		result.Transport = tr.snapshot()
		result.ModelRequests = calls.Load()
		if result.ModelRequests < int64(len(result.Turns)) { result.Failures = append(result.Failures, "fixture-model-request-missing") }
		if o.Attachments && o.Sessions > 0 && result.Transport.Downloads < int64(o.Sessions) { result.Failures = append(result.Failures, "attachment-not-downloaded") }
		if o.TextBytes >= 64<<10 && o.Sessions > 0 && result.Transport.Files < int64(len(result.Turns)) { result.Failures = append(result.Failures, "complete-answer-file-not-delivered") }
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		result.AllocatedBytes, result.Allocations = after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs
		if result.Cleanup { _ = os.RemoveAll(dir) }
		if len(result.Failures) == 0 { result.Status = "passed" }
	}()
	start := time.Now()
	c := config.Defaults()
	c.TenantID, c.AppID = "tenant_fixture", "cli_fixture"
	c.DataDir = filepath.Join(dir, "state")
	c.Runtime.ClaudeExecutable = o.Claude
	c.Runtime.MaxWorkers = max(1, o.Sessions)
	c.Runtime.IdleMS = 600000
	c.Model.BaseURL, c.Model.AllowHTTP = server.URL, true
	c.Model.TokenEnv, c.Model.Model, c.Model.SmallModel = "FEISHU_BASELINE_FIXTURE_TOKEN", "fixture-model", "fixture-model"
	c.Projects = []core.Project{{ID: "fixture", Maintainers: []string{"alice"}, PermissionMode: "acceptEdits"}}
	c.Groups = nil
	for i := 0; i < o.Sessions; i++ { c.Groups = append(c.Groups, core.Group{ChatID: fmt.Sprintf("fixture-chat-%d", i), ProjectID: "fixture", Members: []string{"alice"}}) }
	c.DMs = nil
	c.Limits.UpdateMS = int64(o.UpdateMS)
	c.Limits.MessagesPerMinute = 1000
	st, err = store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil { result.Failures = append(result.Failures, "store-open"); return }
	factory := native.NewFactory(c, func(string, core.Args) {})
	wrapped := func(ctx context.Context, room core.Room, session core.Session, workspace core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		stopping := &atomic.Bool{}
		w, err := factory(ctx, room, session, workspace, func(e core.WorkerEvent) {
			mu.Lock()
			if current := byRoom[room.Key]; current != nil {
				if (e.Type == "text" || e.Type == "thinking") && e.Text != "" && current.first == nil { current.first = timestamp(current.started) }
				if e.Type == "result" {
					current.result, current.success, current.matched, current.usage = timestamp(current.started), e.Success, strings.TrimSpace(e.Text) == reply, usage(e.Usage)
				}
			}
			if (e.Type == "error" || e.Type == "exit") && !stopping.Load() { nativeFailure.Store(true) }
			mu.Unlock()
			emit(e)
		})
		if err != nil { return nil, err }
		mu.Lock(); stopFlags = append(stopFlags, stopping); mu.Unlock()
		workers.Add(1); active.Add(1)
		return &observedWorker{Worker: w, stopping: stopping, onStopped: func() { active.Add(-1) }}, nil
	}
	g = gateway.New(c, st, tr, wrapped, func(string, core.Args) {})
	if err = g.Recover(context.Background()); err != nil { result.Failures = append(result.Failures, "recovery"); return }
	result.ServiceInit = elapsed(start)
	result.MemoryPhases["no-session"] = memorySnapshot(o.Claude)
	sampler=startMemorySampler(o.Claude)
	previous := map[int]core.Session{}
	runTurn := func(roomIndex int, kind string, attach bool) bool {
		input := core.Message{ID: core.NewID(), ChatID: c.Groups[roomIndex].ChatID, ChatType: "group", SenderID: "alice", Text: "Return the local benchmark fixture answer without using tools.", Mentioned: true, Time: time.Now().UnixMilli()}
		if attach { input.Resources = []core.Resource{{Key: "fixture-attachment", Kind: "file", Name: "benchmark-input.txt"}} }
		room, ok := gateway.Route(c, input)
		if !ok { result.Failures = append(result.Failures, "route"); return false }
		current := &observation{id: input.ID, started: time.Now(), usage: usage(nil), completed: make(chan struct{})}
		mu.Lock(); byRoom[room.Key], byID[input.ID] = current, current; mu.Unlock()
		if err := g.Message(context.Background(), input); err != nil { result.Failures = append(result.Failures, "inbound"); return false }
		deadline := time.NewTimer(45*time.Second)
		select { case <-current.completed: case <-deadline.C: }
		deadline.Stop()
		mu.Lock()
		observed := turn{Room: roomIndex, Kind: kind, Success: current.success && current.done != nil && current.matched, Matched: current.matched, First: current.first, Result: current.result, Delivery: current.done, Usage: current.usage}
		delete(byRoom, room.Key); delete(byID, input.ID)
		mu.Unlock()
		result.Turns = append(result.Turns, observed)
		if !observed.Success || observed.First == nil { result.Failures = append(result.Failures, "turn-not-delivered"); return false }
		session, err := st.Session(room.Key)
		old := previous[roomIndex]
		if err != nil || session.CCID == "" || (old.ID != "" && (session.ID != old.ID || session.CCID != old.CCID)) {
			result.Stable = false; result.Failures = append(result.Failures, "session-changed"); return false
		}
		previous[roomIndex] = session
		return true
	}
	for i := 0; i < o.Sessions; i++ {
		if !runTurn(i, "cold-native", o.Attachments) { return }
		for warm := 0; warm < o.WarmTurns; warm++ { if !runTurn(i, "warm-native", false) { return } }
	}
	if int(workers.Load()) != o.Sessions { result.Failures = append(result.Failures, "unexpected-worker-count"); return }
	timer := time.NewTimer(time.Duration(o.IdleMS)*time.Millisecond)
	<-timer.C
	result.Memory = memorySnapshot(o.Claude)
	result.MemoryPhases["warm-idle"] = result.Memory
	if o.Reclaim && o.Sessions > 0 {
		mu.Lock(); for _, flag := range stopFlags { flag.Store(true) }; mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		g.Sweep(ctx, time.Now().UnixMilli()+c.Runtime.IdleMS+1)
		cancel()
		result.Reclaimed = active.Load() == 0
		result.MemoryPhases["after-reclaim"] = memorySnapshot(o.Claude)
		if !result.Reclaimed { result.Failures = append(result.Failures, "idle-reclaim-unconfirmed"); return }
		for i := 0; i < o.Sessions; i++ { if !runTurn(i, "resume-after-reclaim", false) { return } }
		if int(workers.Load()) != 2*o.Sessions { result.Failures = append(result.Failures, "resume-worker-count") }
		result.MemoryPhases["resumed"] = memorySnapshot(o.Claude)
	}
	return
}

func distribution(v []float64) core.Args {
	sort.Float64s(v)
	if len(v) == 0 { return core.Args{"count": 0, "min": nil, "median": nil, "max": nil} }
	median := v[len(v)/2]
	if len(v)%2 == 0 { median = (v[len(v)/2-1]+median)/2 }
	return core.Args{"count": len(v), "min": v[0], "median": median, "max": v[len(v)-1]}
}

// An unconfirmed shutdown contaminates later process-tree samples.
func collectSamples(o settings, run func(int, settings) sample) []sample {
	samples := make([]sample, 0, o.Samples)
	for i := 1; i <= o.Samples; i++ {
		s := run(i, o); samples = append(samples, s)
		if !s.Cleanup { break }
	}
	return samples
}

func scenarios(o settings) []settings {
	if o.Scenario != "matrix" { return []settings{o} }
	out := []settings{}
	add := func(name string, sessions, text, update, delay int, attachments, reclaim bool) {
		v := o
		v.Scenario, v.Sessions, v.TextBytes, v.UpdateMS, v.TransportMS, v.Attachments, v.Reclaim = name, sessions, text, update, delay, attachments, reclaim
		out = append(out, v)
	}
	add("no-session", 0, 0, 1000, 0, false, false)
	add("one-warm", 1, 0, 1000, 0, false, false)
	add("four-warm", 4, 0, 1000, 0, false, false)
	add("chinese-20k-fast", 1, 20<<10, 20, 0, false, false)
	add("chinese-20k-slow", 1, 20<<10, 1000, 100, false, false)
	add("chinese-100k-fast", 1, 100<<10, 20, 0, false, false)
	add("chinese-100k-slow", 1, 100<<10, 1000, 100, false, false)
	add("attachments", 1, 100<<10, 1000, 0, true, false)
	add("after-reclaim", 1, 0, 1000, 0, false, true)
	return out
}

func nativeIdentity(path string) native.VersionCompatibility {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.Command(path, "--version")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if process.Run(ctx, cmd) != nil { return native.Compatibility("") }
	return native.Compatibility(output.String())
}

func main() {
	var o settings
	flag.IntVar(&o.Samples, "samples", 3, "independent samples per scenario (1..10)")
	flag.IntVar(&o.WarmTurns, "warm-turns", 2, "warm continuations per session (0..5)")
	flag.IntVar(&o.IdleMS, "idle-ms", 1000, "idle observation window (100..10000)")
	flag.StringVar(&o.Scenario, "scenario", "warm", "warm or matrix (0/1/4 sessions, Chinese text, attachments, reclaim/resume)")
	flag.IntVar(&o.Sessions, "sessions", 1, "simultaneously retained warm sessions (0..8)")
	flag.IntVar(&o.TextBytes, "text-bytes", 0, "UTF-8 Chinese reply bytes (0 selects short fixture; max 102400)")
	flag.IntVar(&o.UpdateMS, "update-ms", 1000, "gateway streaming update interval (20..5000)")
	flag.IntVar(&o.TransportMS, "transport-ms", 0, "local mock card latency (0..1000)")
	flag.BoolVar(&o.Attachments, "attachments", false, "include a 1 MiB synthetic inbound file and observe outgoing exports")
	flag.BoolVar(&o.Reclaim, "reclaim", false, "reclaim confirmed idle processes and resume the same native IDs")
	flag.StringVar(&o.Claude, "claude", os.Getenv("CLAUDE_NATIVE_PATH"), "native Claude executable")
	flag.Parse()
	if flag.NArg()!=0 || o.Samples<1 || o.Samples>10 || o.WarmTurns<0 || o.WarmTurns>5 || o.IdleMS<100 || o.IdleMS>10000 || o.Sessions<0 || o.Sessions>8 || o.TextBytes<0 || o.TextBytes>100<<10 || o.UpdateMS<20 || o.UpdateMS>5000 || o.TransportMS<0 || o.TransportMS>1000 || (o.Scenario!="warm" && o.Scenario!="matrix") {
		fmt.Fprintln(os.Stderr, "invalid benchmark arguments"); os.Exit(1)
	}
	if o.Claude == "" { o.Claude = "claude" }
	if os.Setenv("FEISHU_BASELINE_FIXTURE_TOKEN", fixtureToken) != nil { os.Exit(1) }
	all := []sample{}
	planned := scenarios(o)
	for _, scenario := range planned {
		samples := collectSamples(scenario, runSample); all = append(all, samples...)
		if len(samples)>0 && !samples[len(samples)-1].Cleanup { break }
	}
	var cold, warm, resumed, stops []float64
	passed := 0
	for _, s := range all {
		if s.Status == "passed" { passed++ }
		if s.Status == "passed" && s.Cleanup { stops = append(stops, s.Stop) }
		for _, t := range s.Turns {
			if t.Success && t.First != nil {
				switch t.Kind { case "cold-native": cold=append(cold,*t.First); case "resume-after-reclaim": resumed=append(resumed,*t.First); default: warm=append(warm,*t.First) }
			}
		}
	}
	expected := o.Samples*len(planned)
	identity := native.VersionCompatibility{Tested:native.TestedVersion,Status:"not_requested"}
	for _, scenario := range planned { if scenario.Sessions>0 { identity=nativeIdentity(o.Claude);break } }
	report := core.Args{"schema":2,"fixture":"deterministic-text-v2","settings":o,"native":identity,"environment":core.Args{"platform":runtime.GOOS,"arch":runtime.GOARCH,"go":runtime.Version(),"logicalCpus":runtime.NumCPU()},"scope":core.Args{"model":"Local deterministic HTTP fixture; synthetic tokens are not model cost.","transport":"In-memory Feishu substitute; no live acceptance. Slow-card latency is synthetic.","memory":"Host includes benchmark, gateway, SQLite and fixture. RSS/Working Set sums can double-count shared pages; unavailable is not zero. Native tools and MCP descendants are grouped, not inferred individually.","allocations":"Process-wide Go allocations include the driver and fixtures; use the isolated render microbenchmarks for the rendering component.","cold":"Fresh native session, not cold OS caches.","liveServiceAccepted":false},"samples":all,"summary":core.Args{"attemptedSamples":len(all),"passedSamples":passed,"failedSamples":len(all)-passed,"unattemptedSamples":expected-len(all),"coldFirstOutputMs":distribution(cold),"warmFirstOutputMs":distribution(warm),"resumeFirstOutputMs":distribution(resumed),"stopMs":distribution(stops)}}
	enc:=json.NewEncoder(os.Stdout); enc.SetIndent("","  ")
	if enc.Encode(report)!=nil || passed!=expected { os.Exit(1) }
}
