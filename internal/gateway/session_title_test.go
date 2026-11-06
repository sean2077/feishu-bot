package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type titleHookWorker struct {
	core.Worker
	rename func(context.Context, string) error
}

func (w *titleHookWorker) Rename(ctx context.Context, title string) error {
	return w.rename(ctx, title)
}

func installTitleHook(f *fixture, rename func(context.Context, string) error) {
	base := f.g.factory
	f.g.factory = func(ctx context.Context, r core.Room, s core.Session, workspace core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		worker, err := base(ctx, r, s, workspace, emit)
		if err != nil {
			return nil, err
		}
		return &titleHookWorker{Worker: worker, rename: rename}, nil
	}
}

func TestSlowTitleDoesNotBlockFirstInputOrRenameCommands(t *testing.T) {
	f := newFixture(t, nil)
	titles := make(chan string, 8)
	ack := make(chan struct{}, 8)
	installTitleHook(f, func(ctx context.Context, title string) error {
		titles <- title
		select {
		case <-ack:
			return nil
		case <-ctx.Done():
			return &core.ControlFailure{State: core.ControlUnconfirmed, Err: ctx.Err()}
		}
	})
	f.send(f.message("first", "Initial title", "member"))
	w := f.worker()
	first := f.delivery(w)
	select {
	case <-titles:
	case <-time.After(time.Second):
		t.Fatal("automatic title was not scheduled")
	}
	for i, title := range []string{"Intermediate name", "Latest name"} {
		done := make(chan error, 1)
		m := f.message(string(rune('a'+i)), "/rename "+title, "owner")
		go func() { done <- f.g.Message(context.Background(), m) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("slow native title held the chat command lock")
		}
	}
	pending, err := f.s.TitleSync(f.room().Key, first.SessionID)
	if err != nil || pending.Desired != "Latest name" || pending.State != "pending" || pending.Confirmed != "" {
		t.Fatal(pending, err)
	}
	ack <- struct{}{}
	select {
	case title := <-titles:
		if title != "Latest name" {
			t.Fatal("coalesced rename sent stale title", title)
		}
	case <-time.After(time.Second):
		t.Fatal("latest title was not synchronized")
	}
	pending, _ = f.s.TitleSync(f.room().Key, first.SessionID)
	if pending.State != "pending" || pending.Confirmed != "" {
		t.Fatal("old automatic title ACK confirmed newer name", pending)
	}
	ack <- struct{}{}
	eventually(t, func() bool {
		title, err := f.s.TitleSync(f.room().Key, first.SessionID)
		return err == nil && title.State == "confirmed" && title.Confirmed == "Latest name"
	})
	f.send(f.message("next", "Continue normal work", "member"))
	if next := f.delivery(w); next.SessionID != first.SessionID || f.starts.Load() != 1 || w.stops.Load() != 0 {
		t.Fatal("title synchronization restarted normal work", next)
	}
}

func TestRenameFailurePreservesSavedNameAndReportsActualOutcome(t *testing.T) {
	for _, tc := range []struct {
		state core.ControlState
		hint  string
	}{
		{core.ControlRejected, "原生端拒绝"},
		{core.ControlUnconfirmed, "未收到确认"},
		{core.ControlNotSent, "尚未发出"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			f := newFixture(t, nil)
			installTitleHook(f, func(_ context.Context, title string) error {
				if title == "同步检查" {
					return &core.ControlFailure{State: tc.state, Err: errors.New("fixture control failure")}
				}
				return nil
			})
			f.send(f.message("first", "Automatic title", "member"))
			w := f.worker()
			delivery := f.delivery(w)
			eventually(t, func() bool {
				title, _ := f.s.TitleSync(f.room().Key, delivery.SessionID)
				return title.State == "confirmed"
			})
			f.send(f.message("rename", "/rename 同步检查", "owner"))
			eventually(t, func() bool {
				title, _ := f.s.TitleSync(f.room().Key, delivery.SessionID)
				return title.State == string(tc.state) && title.Confirmed != "同步检查"
			})
			p, err := f.s.Preferences(f.room().Key, delivery.SessionID)
			if err != nil || p.Name != "同步检查" || w.stops.Load() != 0 {
				t.Fatal("native metadata failure lost saved name or stopped task", p, err)
			}
			eventually(t, func() bool {
				f.tr.mu.Lock()
				defer f.tr.mu.Unlock()
				for _, notice := range f.tr.notices {
					if strings.Contains(notice, "同步检查") && strings.Contains(notice, tc.hint) {
						return true
					}
				}
				return false
			})
		})
	}
}
