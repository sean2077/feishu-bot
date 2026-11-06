package native

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type controlTestPipe struct {
	frames chan args
	gate   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (p *controlTestPipe) Write(data []byte) (int, error) {
	var frame args
	if err := json.Unmarshal(data, &frame); err != nil {
		return 0, err
	}
	select {
	case p.frames <- frame:
	case <-p.closed:
		return 0, io.ErrClosedPipe
	}
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-p.closed:
			return 0, io.ErrClosedPipe
		}
	}
	return len(data), nil
}

func (p *controlTestPipe) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func controlHarness(t *testing.T, blocked bool) (*worker, *controlTestPipe) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &controlTestPipe{frames: make(chan args, 64), closed: make(chan struct{})}
	if blocked {
		p.gate = make(chan struct{})
	}
	w := &worker{ctx: ctx, cancel: cancel, stdin: p, initialized: make(chan struct{}), initID: "initialization", hostRequests: map[string]chan string{}}
	close(w.initialized)
	w.writer = newFrameWriter(ctx, p)
	t.Cleanup(func() {
		cancel()
		_ = p.Close()
		select {
		case <-w.writer.done:
		case <-time.After(time.Second):
			t.Error("single pipe writer did not exit after stdin close")
		}
	})
	return w, p
}

func nextControl(t *testing.T, p *controlTestPipe) args {
	t.Helper()
	select {
	case frame := <-p.frames:
		return frame
	case <-time.After(time.Second):
		t.Fatal("native control was not written")
		return nil
	}
}

func controlResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("native control exceeded its caller deadline")
		return nil
	}
}

func requireControlState(t *testing.T, err error, state core.ControlState) {
	t.Helper()
	var failure *core.ControlFailure
	if !errors.As(err, &failure) || failure.State != state {
		t.Fatalf("control outcome = %v, want %s", err, state)
	}
}

func acknowledge(w *worker, id any, subtype string) {
	w.handleFrame(args{"type": "control_response", "response": args{"request_id": id, "subtype": subtype}})
}

func TestRenameRequiresMatchingAcknowledgement(t *testing.T) {
	w, p := controlHarness(t, false)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Foreground admission can hold worker.mu while emitting. Host control
	// response dispatch must remain independent of that unrelated lock.
	w.mu.Lock()
	go func() { done <- w.Rename(ctx, " Release\n  Notes ") }()
	frame := nextControl(t, p)
	if object(frame["request"])["title"] != "Release Notes" {
		t.Fatal(frame)
	}
	acknowledge(w, "unknown-request", "success")
	select {
	case err := <-done:
		t.Fatalf("pipe write or another request was treated as an ACK: %v", err)
	default:
	}
	acknowledge(w, frame["request_id"], "success")
	acknowledge(w, frame["request_id"], "error")
	if err := controlResult(t, done); err != nil {
		t.Fatal(err)
	}
	w.mu.Unlock()
	go func() { done <- w.Rename(ctx, "Second title") }()
	second := nextControl(t, p)
	acknowledge(w, frame["request_id"], "success")
	acknowledge(w, second["request_id"], "error")
	requireControlState(t, controlResult(t, done), core.ControlRejected)
	if w.ctx.Err() != nil {
		t.Fatal("title rejection cancelled a healthy native worker")
	}
}

func TestControlDeadlineCoversBlockedPipeAndQueuedWrite(t *testing.T) {
	w, p := controlHarness(t, true)
	first := make(chan error, 1)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	go func() { first <- w.Rename(firstCtx, "blocked") }()
	old := nextControl(t, p)
	cancelFirst()
	requireControlState(t, controlResult(t, first), core.ControlUnconfirmed)
	queued := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	go func() { queued <- w.Rename(ctx, "must not be written after timeout") }()
	err := controlResult(t, queued)
	requireControlState(t, err, core.ControlNotSent)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller deadline was not retained", err)
	}
	w.hostMu.Lock()
	remaining := len(w.hostRequests)
	w.hostMu.Unlock()
	if remaining != 0 {
		t.Fatal("expired host control waiters leaked", remaining)
	}
	close(p.gate)
	next := make(chan error, 1)
	go func() { next <- w.Rename(context.Background(), "current") }()
	frame := nextControl(t, p)
	if object(frame["request"])["title"] != "current" {
		t.Fatal("cancelled queued control was written later", frame)
	}
	acknowledge(w, old["request_id"], "success")
	acknowledge(w, frame["request_id"], "success")
	if err := controlResult(t, next); err != nil || w.ctx.Err() != nil {
		t.Fatal("title timeout stopped the worker", err, w.ctx.Err())
	}
}

func TestControlInitializationDeadlineAndWaiterLimit(t *testing.T) {
	w, p := controlHarness(t, false)
	w.initialized = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	requireControlState(t, w.Rename(ctx, "before initialization"), core.ControlNotSent)
	select {
	case frame := <-p.frames:
		t.Fatal("rename overtook initialization", frame)
	default:
	}
	close(w.initialized)
	for i := 0; i < maxHostControls; i++ {
		w.hostRequests[core.NewID()] = make(chan string, 1)
	}
	requireControlState(t, w.Rename(context.Background(), "overflow"), core.ControlNotSent)
	if len(w.hostRequests) != maxHostControls {
		t.Fatal("host control capacity changed")
	}
}

func TestNativeTitleFailureDoesNotEndNormalTask(t *testing.T) {
	f := newFixture(t)
	f.start()
	requireControlState(t, f.worker.Rename(context.Background(), "fixture-reject-title"), core.ControlRejected)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	requireControlState(t, f.worker.Rename(ctx, "fixture-missing-ack"), core.ControlUnconfirmed)
	delivery := f.send("normal work after title failure")
	result := f.event("result")
	if !result.Success || result.UUID != delivery.UUID {
		t.Fatal("normal task lost after title failure", result)
	}
	if err := f.worker.Rename(context.Background(), "confirmed later title"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.all {
		if event.Type == "error" || event.Type == "exit" {
			t.Fatal("metadata failure changed worker lifecycle", event)
		}
	}
}
