package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

type actionCapture struct {
	mu    sync.Mutex
	calls int
	args  [][]string
	env   []map[string]string
	run   func(context.Context, string, []string, string, map[string]string, int64) (int, string, error)
}

func (c *actionCapture) invoke(ctx context.Context, executable string, args []string, cwd string, env map[string]string, timeout int64) (int, string, error) {
	c.mu.Lock()
	c.calls++
	c.args = append(c.args, append([]string(nil), args...))
	copiedEnv := map[string]string{}
	for key, value := range env {
		copiedEnv[key] = value
	}
	c.env = append(c.env, copiedEnv)
	run := c.run
	c.mu.Unlock()
	if run != nil {
		return run(ctx, executable, args, cwd, env, timeout)
	}
	return 0, "retrieved context", nil
}

func (c *actionCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *actionCapture) last() ([]string, map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.args) == 0 {
		return nil, nil
	}
	return c.args[len(c.args)-1], c.env[len(c.env)-1]
}

func preContextFixture(t *testing.T, mutate func(*core.Config, *core.Project)) *fixture {
	t.Helper()
	return newFixture(t, func(c *core.Config) {
		p := &c.Projects[0]
		p.Actions = []core.ActionSpec{{
			ID:          "search",
			Description: "read-only fixture search",
			Executable:  "fixture-search",
			Args:        []string{"search", "--limit", "5", "--", "{{question}}"},
			ReadOnly:    true,
			TimeoutMS:   10000,
			Parameters: map[string]core.Parameter{
				"question": {Description: "one user question", Pattern: `^[^\x00-\x1f\x7f]+$`, MaxLength: 500},
			},
			Vars: map[string]string{"FIXTURE_PROFILE": "demo"},
		}}
		p.PreContext = &core.ProjectPreContext{Action: "search", Parameter: "question", TimeoutMS: 2000, MaxChars: 100}
		if mutate != nil {
			mutate(c, p)
		}
	})
}

func preContextAudits(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	rows, err := f.s.DB.Query("SELECT detail FROM audit WHERE event='pre_context_failed' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		result = append(result, detail)
	}
	return result
}

func turnResultDetail(t *testing.T, f *fixture, uuid string) map[string]any {
	t.Helper()
	var raw string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='turn_result' AND json_extract(detail,'$.turn')=? ORDER BY seq DESC LIMIT 1", uuid).Scan(&raw); err != nil {
		t.Fatalf("turn_result %s: %v", uuid, err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func waitPreContextIdle(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := f.g.getSlot(f.room().Key)
		if s == nil {
			return
		}
		s.mu.Lock()
		busy, deliveries, active, compacting := s.busy, len(s.deliveries), s.activeRequests, s.compacting
		s.mu.Unlock()
		if !busy && deliveries == 0 && active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not idle: busy=%t deliveries=%d activeRequests=%d compacting=%q", busy, deliveries, active, compacting)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPreContextOnlyRunsForNewTurnAndInjectsReadOnlyResult(t *testing.T) {
	f := preContextFixture(t, nil)
	capture := &actionCapture{}
	f.g.actionRunner = capture.invoke

	f.send(f.message("first", "  first\nline  ", "member"))
	w := f.worker()
	first := f.delivery(w)
	if first.PreContext == nil || first.PreContext.Source != "search" || first.PreContext.Text != "retrieved context" || first.PreContext.Truncated {
		t.Fatalf("first delivery preContext = %+v", first.PreContext)
	}
	args, env := capture.last()
	if strings.Join(args, "\x00") != strings.Join([]string{"search", "--limit", "5", "--", "first line"}, "\x00") {
		t.Fatalf("action argv = %#v", args)
	}
	if env["FIXTURE_PROFILE"] != "demo" || env["FEISHU_REQUEST_MESSAGE_ID"] != "first" || env["FEISHU_REQUEST_SENDER_OPEN_ID"] != "member" {
		t.Fatalf("action environment = %#v", env)
	}
	var runState string
	if err := f.s.DB.QueryRow("SELECT state FROM action_runs WHERE action='search'").Scan(&runState); err != nil || runState != "done" {
		t.Fatalf("action run state = %q, %v", runState, err)
	}

	f.send(f.message("second", "still the same turn", "member"))
	second := f.delivery(w)
	if second.PreContext != nil {
		t.Fatalf("in-turn input received preContext: %+v", second.PreContext)
	}
	f.send(f.message("supplement", "/btw add detail", "member"))
	supplement := f.delivery(w)
	if supplement.PreContext != nil {
		t.Fatalf("supplement received preContext: %+v", supplement.PreContext)
	}
	f.send(f.message("status", "/status", "member"))
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		return len(f.tr.notices) > 0
	})
	if capture.count() != 1 {
		t.Fatalf("commands/in-turn inputs ran preContext %d times", capture.count())
	}

	w.emit(core.WorkerEvent{Type: "result", UUID: first.UUID, UUIDs: []string{first.UUID, second.UUID, supplement.UUID}, ResultID: core.NewID(), Success: true, Source: "foreground", Text: "first turn complete"})
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='turn_result' AND json_extract(detail,'$.turn')=?", first.UUID).Scan(&n) == nil && n > 0
	})
	if detail := turnResultDetail(t, f, first.UUID); detail["preContext"] != "used" || detail["preContextMs"].(float64) < 0 {
		t.Fatalf("used turn diagnostics = %#v", detail)
	}
	waitPreContextIdle(t, f)

	f.send(f.message("compact", "/compact keep context", "owner"))
	compact := f.delivery(w)
	if compact.Command == nil || compact.PreContext != nil {
		t.Fatalf("compact delivery = %+v", compact)
	}
	if capture.count() != 1 {
		t.Fatalf("compact ran preContext")
	}
	w.emit(core.WorkerEvent{Type: "result", UUID: compact.UUID, UUIDs: []string{compact.UUID}, ResultID: core.NewID(), Success: true, Compact: "compacted", Text: "compacted"})
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='turn_result' AND json_extract(detail,'$.turn')=?", compact.UUID).Scan(&n) == nil && n > 0
	})
	if detail := turnResultDetail(t, f, compact.UUID); detail["preContext"] != "skipped" || detail["preContextMs"].(float64) != 0 {
		t.Fatalf("compact diagnostics = %#v", detail)
	}
	waitPreContextIdle(t, f)

	f.send(f.message("third", "a new question", "member"))
	third := f.delivery(w)
	if third.PreContext == nil || capture.count() != 2 {
		t.Fatalf("new turn preContext calls=%d delivery=%+v", capture.count(), third.PreContext)
	}
}

func TestPreContextRunsInParallelWithWorkerStartup(t *testing.T) {
	f := preContextFixture(t, nil)
	actionStarted := make(chan struct{})
	workerStarted := make(chan struct{})
	releaseAction := make(chan struct{})
	releaseWorker := make(chan struct{})
	f.g.actionRunner = func(ctx context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
		close(actionStarted)
		select {
		case <-releaseAction:
			return 0, "parallel context", nil
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
	baseFactory := f.g.factory
	f.g.factory = func(ctx context.Context, r core.Room, s core.Session, w core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		close(workerStarted)
		select {
		case <-releaseWorker:
			return baseFactory(ctx, r, s, w, emit)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	start := time.Now()
	f.send(f.message("parallel", "parallel question", "member"))
	wait := func(name string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not start concurrently", name)
		}
	}
	wait("preContext action", actionStarted)
	wait("worker startup", workerStarted)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("preContext and worker startup did not overlap: %s", elapsed)
	}
	close(releaseAction)
	close(releaseWorker)
	w := f.worker()
	delivery := f.delivery(w)
	if delivery.PreContext == nil || delivery.PreContext.Text != "parallel context" {
		t.Fatalf("parallel delivery preContext = %+v", delivery.PreContext)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("parallel preContext and startup took additively long: %s", elapsed)
	}
}

func TestPreContextKeepsShellMetacharactersAsOneArgvAndAllowsLeadingDash(t *testing.T) {
	f := preContextFixture(t, nil)
	capture := &actionCapture{}
	f.g.actionRunner = capture.invoke
	f.send(f.message("shell", "  --help\n$(echo injected) ; rm -rf /tmp/example\t`quoted` ", "member"))
	w := f.worker()
	_ = f.delivery(w)
	args, _ := capture.last()
	const want = "--help $(echo injected) ; rm -rf /tmp/example `quoted`"
	if len(args) != 5 || args[4] != want {
		t.Fatalf("shell metacharacters were split or transformed: %#v", args)
	}
	if strings.Contains(args[4], "\n") || strings.Contains(args[4], "\t") {
		t.Fatalf("control characters survived normalization: %q", args[4])
	}
}

func TestPreContextTruncatesOutputOnRuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		output    string
		max       int
		truncated bool
		want      string
	}{
		{"truncated", "一二三四五六七八九十甲乙", 10, true, "一二三四五六七八九十"},
		{"exact", "一二三四五六七八九十", 10, false, "一二三四五六七八九十"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := preContextFixture(t, func(_ *core.Config, p *core.Project) { p.PreContext.MaxChars = tc.max })
			f.g.actionRunner = func(context.Context, string, []string, string, map[string]string, int64) (int, string, error) {
				return 0, tc.output, nil
			}
			f.send(f.message("truncate", "question", "member"))
			w := f.worker()
			delivery := f.delivery(w)
			if delivery.PreContext == nil || delivery.PreContext.Text != tc.want || delivery.PreContext.Truncated != tc.truncated {
				t.Fatalf("preContext = %+v", delivery.PreContext)
			}
			if !utf8.ValidString(delivery.PreContext.Text) || utf8.RuneCountInString(delivery.PreContext.Text) != tc.max {
				t.Fatalf("preContext is not a complete UTF-8 truncation: %q", delivery.PreContext.Text)
			}
		})
	}
}

func TestPreContextFailuresFailOpenAndAudit(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		timeout int64
		runner  func(context.Context) (int, string, error)
		reason  string
		calls   int
	}{
		{"error", "SECRET_QUERY_MARKER", 2000, func(context.Context) (int, string, error) {
			return 0, "", errors.New("fixture action failed")
		}, "execution", 1},
		{"exit", "question", 2000, func(context.Context) (int, string, error) {
			return 3, "partial output", nil
		}, "exit", 1},
		{"empty", "question", 2000, func(context.Context) (int, string, error) {
			return 0, " \n\t ", nil
		}, "empty", 1},
		{"parameter", "\x01", 2000, func(context.Context) (int, string, error) {
			t.Error("invalid parameter reached the action runner")
			return 0, "unexpected", nil
		}, "parameter", 0},
		{"timeout", "question", 50, func(ctx context.Context) (int, string, error) {
			<-ctx.Done()
			return 0, "", ctx.Err()
		}, "timeout", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := preContextFixture(t, func(_ *core.Config, p *core.Project) { p.PreContext.TimeoutMS = tc.timeout })
			f.g.actionRunner = func(ctx context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
				return tc.runner(ctx)
			}
			start := time.Now()
			f.send(f.message("fail", tc.text, "member"))
			w := f.worker()
			delivery := f.delivery(w)
			if delivery.PreContext != nil {
				t.Fatalf("failed preContext was injected: %+v", delivery.PreContext)
			}
			eventually(t, func() bool { return len(preContextAudits(t, f)) == 1 })
			audit := preContextAudits(t, f)[0]
			if audit["project"] != "project" || audit["action"] != "search" || audit["reason"] != tc.reason {
				t.Fatalf("pre_context_failed = %#v", audit)
			}
			if audit["durationMs"].(float64) < 0 {
				t.Fatalf("durationMs = %v", audit["durationMs"])
			}
			raw, _ := json.Marshal(audit)
			if strings.Contains(string(raw), "SECRET_QUERY_MARKER") {
				t.Fatalf("failure audit leaked the query: %s", raw)
			}
			if tc.name == "timeout" && time.Since(start) > 2*time.Second {
				t.Fatalf("timeout waited too long: %s", time.Since(start))
			}
			f.finish(w, delivery, "fail-open reply")
			eventually(t, func() bool {
				var n int
				return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='turn_result' AND json_extract(detail,'$.turn')=?", delivery.UUID).Scan(&n) == nil && n > 0
			})
			detail := turnResultDetail(t, f, delivery.UUID)
			if detail["preContext"] != "failed" || detail["preContextMs"].(float64) < 0 {
				t.Fatalf("failed turn diagnostics = %#v", detail)
			}
			waitPreContextIdle(t, f)
			if tc.name == "error" {
				f.send(f.message("fail-again", "another question", "member"))
				again := f.delivery(w)
				eventually(t, func() bool { return len(preContextAudits(t, f)) == 2 })
				if again.PreContext != nil {
					t.Fatal("second failure was not fail-open")
				}
			}
		})
	}
}

func TestPreContextSkippedWithoutConfiguration(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("plain", "question", "member"))
	w := f.worker()
	delivery := f.delivery(w)
	if delivery.PreContext != nil {
		t.Fatalf("unconfigured preContext was injected: %+v", delivery.PreContext)
	}
	f.finish(w, delivery, "done")
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='turn_result' AND json_extract(detail,'$.turn')=?", delivery.UUID).Scan(&n) == nil && n > 0
	})
	detail := turnResultDetail(t, f, delivery.UUID)
	if detail["preContext"] != "skipped" || detail["preContextMs"].(float64) != 0 {
		t.Fatalf("skipped turn diagnostics = %#v", detail)
	}
}

func TestPreContextTriggerTextUsesFirstCaptionNotImagePlaceholder(t *testing.T) {
	imageOnly := core.Delivery{Message: core.Message{Text: "[图片]", ImageOnly: true}}
	if got := preContextTriggerText(imageOnly); got != "" {
		t.Fatalf("image-only trigger text = %q", got)
	}
	captioned := core.Delivery{Message: core.Message{Text: "caption for image", ImageOnly: false}}
	if got := preContextTriggerText(captioned); got != "caption for image" {
		t.Fatalf("captioned trigger text = %q", got)
	}
	if got := normalizePreContextText(" a\n\tb\x01 c  "); got != "a b c" {
		t.Fatalf("normalized text = %q", got)
	}
}

func TestPreContextImageOnlyBatchSkipsWithoutUserText(t *testing.T) {
	f := preContextFixture(t, func(c *core.Config, _ *core.Project) {
		c.Limits.ImageBatchMS = 5000
		c.Limits.ImageBatchMaxImages = 2
	})
	capture := &actionCapture{}
	f.g.actionRunner = capture.invoke
	f.send(f.image("batch-first", "member"))
	eventually(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		_, ok := f.g.imageTimers[f.room().Key]
		return ok
	})
	f.send(f.image("batch-second", "member"))
	w := f.worker()
	delivery := f.delivery(w)
	if capture.count() != 0 || delivery.PreContext != nil {
		t.Fatalf("image-only batch ran preContext: calls=%d value=%+v", capture.count(), delivery.PreContext)
	}
	if delivery.BatchInputs == nil || len(delivery.BatchInputs) != 2 {
		t.Fatalf("image batch delivery = %+v", delivery)
	}
}
