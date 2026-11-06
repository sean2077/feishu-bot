package native

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
)

const nativeWriteQueue = 8

var errWorkerUnavailable = errors.New("native worker unavailable")

type frameWrite struct {
	ctx   context.Context
	data  []byte
	state atomic.Int32 // queued (0), started (1), cancelled before writing (2)
	done  chan error
}

// frameWriter owns the only pipe-writing goroutine. A deadline releases the
// caller even when the OS pipe is blocked; Stop closes stdin and joins this
// goroutine. Queued, cancelled frames are never written after their deadline.
type frameWriter struct {
	ctx   context.Context
	pipe  io.Writer
	queue chan *frameWrite
	done  chan struct{}
}

func newFrameWriter(ctx context.Context, pipe io.Writer) *frameWriter {
	w := &frameWriter{ctx: ctx, pipe: pipe, queue: make(chan *frameWrite, nativeWriteQueue), done: make(chan struct{})}
	go w.run()
	return w
}

func (w *frameWriter) run() {
	defer close(w.done)
	for {
		select {
		case <-w.ctx.Done():
			return
		case job := <-w.queue:
			if job.ctx.Err() != nil || w.ctx.Err() != nil {
				job.state.CompareAndSwap(0, 2)
				continue
			}
			if !job.state.CompareAndSwap(0, 1) {
				continue
			}
			n, err := w.pipe.Write(job.data)
			if err == nil && n != len(job.data) {
				err = io.ErrShortWrite
			}
			job.done <- err
		}
	}
}

func (w *frameWriter) write(ctx context.Context, data []byte) (started bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if w.ctx.Err() != nil {
		return false, errWorkerUnavailable
	}
	job := &frameWrite{ctx: ctx, data: data, done: make(chan error, 1)}
	select {
	case w.queue <- job:
	case <-ctx.Done():
		return false, ctx.Err()
	case <-w.ctx.Done():
		return false, errWorkerUnavailable
	}
	select {
	case err := <-job.done:
		return true, err
	case <-ctx.Done():
		err = ctx.Err()
	case <-w.ctx.Done():
		err = errWorkerUnavailable
	}
	// If completion raced cancellation, retain the observed write result. The
	// CAS makes the remaining distinction exact even while the writer is queued.
	select {
	case err := <-job.done:
		return true, err
	default:
	}
	job.state.CompareAndSwap(0, 2)
	return job.state.Load() == 1, err
}
