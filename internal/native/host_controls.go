package native

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const (
	nativeControlTimeout = 5 * time.Second
	maxHostControls      = 32
)

// Rename only succeeds after the matching native acknowledgement. Its deadline
// covers initialization, writer admission, the pipe write and the response.
// Rejection or a missing acknowledgement does not stop the conversation.
func (w *worker) Rename(ctx context.Context, title string) error {
	ctx, cancel := context.WithTimeout(ctx, nativeControlTimeout)
	defer cancel()
	notSent := func(err error) error { return &core.ControlFailure{State: core.ControlNotSent, Err: err} }
	if err := ctx.Err(); err != nil {
		return notSent(err)
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return notSent(errors.New("native session title required"))
	}
	select {
	case <-w.initialized:
	case <-ctx.Done():
		return notSent(ctx.Err())
	case <-w.ctx.Done():
		return notSent(errWorkerUnavailable)
	}
	if w.ctx.Err() != nil {
		return notSent(errWorkerUnavailable)
	}
	id := core.NewID()
	ack := make(chan string, 1)
	w.hostMu.Lock()
	if len(w.hostRequests) >= maxHostControls {
		w.hostMu.Unlock()
		return notSent(errors.New("native control queue full"))
	}
	w.hostRequests[id] = ack
	w.hostMu.Unlock()
	defer func() {
		w.hostMu.Lock()
		delete(w.hostRequests, id)
		w.hostMu.Unlock()
	}()
	started, err := w.writeContext(ctx, args{"type": "control_request", "request_id": id, "request": args{"subtype": "rename_session", "title": title, "source": "host"}})
	if err != nil {
		select {
		case response := <-ack:
			return controlOutcome(response)
		default:
		}
		if !started {
			return notSent(err)
		}
		return &core.ControlFailure{State: core.ControlUnconfirmed, Err: err}
	}
	select {
	case response := <-ack:
		return controlOutcome(response)
	case <-ctx.Done():
		err = ctx.Err()
	case <-w.ctx.Done():
		err = errWorkerUnavailable
	}
	select {
	case response := <-ack:
		return controlOutcome(response)
	default:
		return &core.ControlFailure{State: core.ControlUnconfirmed, Err: err}
	}
}

func controlOutcome(subtype string) error {
	if subtype == "success" {
		return nil
	}
	if subtype == "error" {
		return &core.ControlFailure{State: core.ControlRejected, Err: errors.New("native process rejected the title update")}
	}
	return &core.ControlFailure{State: core.ControlUnconfirmed, Err: errors.New("invalid native control acknowledgement")}
}

func (w *worker) resolveHostControl(response args) {
	id := text(response["request_id"])
	w.hostMu.Lock()
	ack := w.hostRequests[id]
	if ack != nil {
		delete(w.hostRequests, id)
		ack <- text(response["subtype"])
	}
	w.hostMu.Unlock()
	// Duplicate, unknown and expired replies do not recreate waiters or change
	// title state. Provider error text may contain private context; omit it.
}
