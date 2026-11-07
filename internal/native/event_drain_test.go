package native

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestProcessStopAndFinalEventCompletionAreSeparateBarriers(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.callback = func(ev core.WorkerEvent) {
		if ev.Type == "exit" {
			close(entered)
			<-release
		}
	}
	f.start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Waiting for the process must remain safe even from an event callback.
	if err := f.worker.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("exit callback did not run")
	}
	events, ok := f.worker.(core.WorkerEvents)
	if !ok {
		t.Fatal("native worker exposes no event completion barrier")
	}
	cancelled, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if err := events.WaitEvents(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("event completion ignored pending callback: %v", err)
	}
	once.Do(func() { close(release) })
	if err := events.WaitEvents(ctx); err != nil {
		t.Fatalf("completed callback did not release barrier: %v", err)
	}
}
