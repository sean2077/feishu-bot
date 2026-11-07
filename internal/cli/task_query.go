package cli

import (
	"context"
	"time"
)

// A Task Scheduler COM read cannot be interrupted by context cancellation.
// Bound the caller's wait; the read-only operation owns cleanup on completion.
func awaitTaskQuery(ctx context.Context, query func(context.Context) (bool, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	type result struct {
		present bool
		err     error
	}
	ready := make(chan result, 1)
	go func() {
		present, err := query(ctx)
		ready <- result{present, err}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case r := <-ready:
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return r.present, r.err
	}
}
