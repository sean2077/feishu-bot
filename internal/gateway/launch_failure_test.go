package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func launchFailureNotices(f *fixture) int {
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	count := 0
	for _, notice := range f.tr.notices {
		if strings.Contains(notice, "运行启动或交付失败") {
			count++
		}
	}
	return count
}

func TestLaunchFailureNoticeIsNotRepeatedForSameInput(t *testing.T) {
	f := newFixture(t, nil)
	var attempts atomic.Int64
	f.g.factory = func(context.Context, core.Room, core.Session, core.Workspace, func(core.WorkerEvent)) (core.Worker, error) {
		attempts.Add(1)
		return nil, errors.New("settings file invalid")
	}
	f.send(f.message("launch-fail", "hello", "member"))
	eventually(t, func() bool { return attempts.Load() >= 1 })
	eventually(t, func() bool { return launchFailureNotices(f) == 1 })
	if got := f.status("launch-fail"); got != "queued" {
		t.Fatalf("failed launch changed the saved input: %q", got)
	}

	before := attempts.Load()
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	eventually(t, func() bool { return attempts.Load() > before })
	time.Sleep(50 * time.Millisecond)
	if got := launchFailureNotices(f); got != 1 {
		t.Fatalf("periodic retry posted %d failure notices", got)
	}

	// A new input may produce one new notice, but not another one per retry.
	f.send(f.message("launch-fail-2", "again", "member"))
	eventually(t, func() bool { return launchFailureNotices(f) == 2 })
	before = attempts.Load()
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	eventually(t, func() bool { return attempts.Load() > before })
	time.Sleep(50 * time.Millisecond)
	if got := launchFailureNotices(f); got != 2 {
		t.Fatalf("retry after a new input posted %d failure notices", got)
	}
}
