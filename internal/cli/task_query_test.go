package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTaskQueryCancellationDoesNotWaitForUninterruptibleRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		present, err := awaitTaskQuery(ctx, func(context.Context) (bool, error) {
			close(started)
			<-release
			defer close(finished)
			return false, nil // A late missing result is not a valid snapshot.
		})
		if present {
			done <- errors.New("reported present after cancellation")
			return
		}
		done <- err
	}()
	<-started
	defer func() {
		close(release)
		<-finished
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reported an authoritative missing result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("query kept caller blocked after cancellation")
	}
}

func TestTaskQueryRejectsCancelledAndUnknownResults(t *testing.T) {
	t.Run("already-cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := awaitTaskQuery(ctx, func(context.Context) (bool, error) {
			t.Error("started a query after cancellation")
			return false, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("cancelled-at-result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		present, err := awaitTaskQuery(ctx, func(context.Context) (bool, error) {
			cancel()
			return true, nil
		})
		if present || !errors.Is(err, context.Canceled) {
			t.Fatal(present, err)
		}
	})
	t.Run("query-failure", func(t *testing.T) {
		failure := errors.New("scheduler unavailable")
		present, err := awaitTaskQuery(context.Background(), func(context.Context) (bool, error) { return false, failure })
		if present || !errors.Is(err, failure) {
			t.Fatal("query error became task absence", present, err)
		}
	})
}
