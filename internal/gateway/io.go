package gateway

import (
	"context"
	"sync"
	"time"
)

type ioResult[T any] struct {
	value T
	err   error
}

// The adapter may finish remotely after cancellation. Only the caller writes
// durable state, so a late result cannot reopen a closed store or claim success.
func provider[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	done := make(chan ioResult[T], 1)
	go func() { v, e := fn(); done <- ioResult[T]{v, e} }()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		select {
		case r := <-done:
			return r.value, r.err
		default:
			return zero, ctx.Err()
		}
	}
}
func providerVoid(ctx context.Context, fn func() error) error {
	_, err := provider(ctx, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// Entries exist only while callers hold or await the lock.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*lockRef
}
type lockRef struct {
	token chan struct{}
	users int
}

func (k *keyedLocks) acquire(id string) *lockRef {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.locks == nil {
		k.locks = map[string]*lockRef{}
	}
	l := k.locks[id]
	if l == nil {
		l = &lockRef{token: make(chan struct{}, 1)}
		l.token <- struct{}{}
		k.locks[id] = l
	}
	l.users++
	return l
}
func (k *keyedLocks) releaseRef(id string, l *lockRef) {
	k.mu.Lock()
	l.users--
	if l.users == 0 {
		delete(k.locks, id)
	}
	k.mu.Unlock()
}
func (k *keyedLocks) lockContext(ctx context.Context, id string) (func(), error) {
	l := k.acquire(id)
	select {
	case <-l.token:
		return func() { l.token <- struct{}{}; k.releaseRef(id, l) }, nil
	case <-ctx.Done():
		k.releaseRef(id, l)
		return nil, ctx.Err()
	}
}
func (k *keyedLocks) lock(id string) func() {
	release, _ := k.lockContext(context.Background(), id)
	return release
}

// lockWithin acquires mu unless ctx ends first.
func lockWithin(ctx context.Context, mu *sync.Mutex) bool {
	for {
		if mu.TryLock() {
			if ctx.Err() != nil {
				mu.Unlock()
				return false
			}
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}
