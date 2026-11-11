package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
)

// turnTiming collects the stages of one native turn. The result audit uses the
// stages up to the native result; delivery timing is emitted separately once a
// delivery attempt ends so a failed attempt cannot make the result disappear.
type turnTiming struct {
	turn         string
	session      string
	queuedAt     time.Time
	dispatchedAt time.Time
	acceptedAt   time.Time
	firstAt      time.Time
	startupMs    int64
	reused       bool
	preContext   string
	preContextMs int64
	tools        time.Duration
	toolStarts   map[string]time.Time
	toolNames    map[string]string
}

type turnDeliveryReport struct {
	turn, session string
	startedAt     time.Time
}

func deliveryQueuedAt(d core.Delivery) time.Time {
	if d.QueuedAt > 0 {
		return time.UnixMilli(d.QueuedAt)
	}
	if d.Message.Time > 0 {
		return time.UnixMilli(d.Message.Time)
	}
	return time.Time{}
}

func msBetween(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func newTurnTiming(s *slot, d core.Delivery) *turnTiming {
	t := &turnTiming{
		turn:         d.UUID,
		session:      s.session.ID,
		queuedAt:     deliveryQueuedAt(d),
		dispatchedAt: time.Now(),
		reused:       s.inputsServed > 0,
		preContext:   preContextSkipped,
		toolStarts:   map[string]time.Time{},
		toolNames:    map[string]string{},
	}
	if !t.reused {
		t.startupMs = s.startupMs
	}
	return t
}

func turnResultTiming(t *turnTiming) core.Args {
	if t == nil {
		return core.Args{}
	}
	firstBase := t.acceptedAt
	if firstBase.IsZero() {
		firstBase = t.dispatchedAt
	}
	return core.Args{
		"turn":         t.turn,
		"queueMs":      msBetween(t.queuedAt, t.dispatchedAt),
		"startupMs":    t.startupMs,
		"reused":       t.reused,
		"preContext":   t.preContext,
		"preContextMs": t.preContextMs,
		"sendMs":       msBetween(t.dispatchedAt, t.acceptedAt),
		"firstMs":      msBetween(firstBase, t.firstAt),
		"toolsMs":      t.tools.Milliseconds(),
	}
}

func (g *Gateway) rememberTurnDelivery(output string, report *turnDeliveryReport) {
	if output == "" || report == nil {
		return
	}
	g.turnMu.Lock()
	defer g.turnMu.Unlock()
	if g.turnReports == nil {
		g.turnReports = map[string]*turnDeliveryReport{}
	}
	if _, exists := g.turnReports[output]; !exists {
		g.turnReportOrder = append(g.turnReportOrder, output)
	}
	g.turnReports[output] = report
	for len(g.turnReportOrder) > 1024 {
		old := g.turnReportOrder[0]
		g.turnReportOrder = g.turnReportOrder[1:]
		delete(g.turnReports, old)
	}
}

func (g *Gateway) turnDeliveryReport(id string) *turnDeliveryReport {
	g.turnMu.Lock()
	defer g.turnMu.Unlock()
	return g.turnReports[id]
}

func (g *Gateway) forgetTurnDelivery(id string) {
	g.turnMu.Lock()
	defer g.turnMu.Unlock()
	delete(g.turnReports, id)
	for i, value := range g.turnReportOrder {
		if value == id {
			g.turnReportOrder = append(g.turnReportOrder[:i], g.turnReportOrder[i+1:]...)
			break
		}
	}
}

func (g *Gateway) auditTurnDelivery(id string, o outgoing, attempt int, started time.Time) {
	if o.Kind != "reply" {
		return
	}
	state := "unknown"
	if current, err := g.output(id); err == nil {
		state = current.State
	}
	args := core.Args{
		"output":      id,
		"session":     o.Session,
		"attempt":     attempt,
		"outboxState": state,
		"deliverMs":   time.Since(started).Milliseconds(),
	}
	if report := g.turnDeliveryReport(id); report != nil {
		args["turn"] = report.turn
		if state == "done" && !report.startedAt.IsZero() {
			args["totalMs"] = time.Since(report.startedAt).Milliseconds()
		}
	}
	g.audit(o.Room, "turn_delivery", args)
	if state == "done" || state == "cancelled" {
		g.forgetTurnDelivery(id)
	}
}

type workerDiagnostics interface {
	SetStopReason(string)
	SetTurnActive(bool)
}

func setWorkerTurn(worker core.Worker, active bool) {
	if w, ok := worker.(workerDiagnostics); ok {
		w.SetTurnActive(active)
	}
}

func setWorkerStopReason(worker core.Worker, reason string) {
	if w, ok := worker.(workerDiagnostics); ok {
		w.SetStopReason(reason)
	}
}

func stopWorkerReason(ctx context.Context, worker core.Worker, reason string) error {
	setWorkerStopReason(worker, reason)
	return stopWorker(ctx, worker)
}

type dispatchFailure struct {
	phase, reason string
	err           error
}

func (e *dispatchFailure) Error() string { return e.err.Error() }
func (e *dispatchFailure) Unwrap() error { return e.err }

func failurePhase(err error) string {
	var d *dispatchFailure
	if errors.As(err, &d) {
		return d.phase
	}
	return "launch"
}

func failureReasonOf(err error) string {
	var d *dispatchFailure
	if errors.As(err, &d) {
		return d.reason
	}
	return failureReason(err)
}

// failureReason returns a fixed, path-free token suitable for logs.
func failureReason(err error) string {
	if err == nil {
		return "other"
	}
	switch {
	case errors.Is(err, store.ErrLaunchIdentityChanged):
		return "launch_identity"
	case errors.Is(err, config.ErrModelProfileUnavailable):
		return "model_profile"
	case errors.Is(err, process.ErrUnconfirmed):
		return "process_unconfirmed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "settings file"), strings.Contains(message, "project settings"):
		return "settings_file"
	case strings.Contains(message, "start claude native executable"):
		return "process_start"
	case strings.Contains(message, "initialization timed out"):
		return "timeout"
	case strings.Contains(message, "native session id changed"):
		return "launch_identity"
	case strings.Contains(message, "workspace"), strings.Contains(message, "cwd"):
		return "workspace"
	}
	return "other"
}

func workerFailureReason(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "timed out"), strings.Contains(lower, "timeout"):
		return "timeout"
	case strings.Contains(lower, "settings"):
		return "settings_file"
	case strings.Contains(lower, "session id"):
		return "launch_identity"
	}
	return "other"
}
