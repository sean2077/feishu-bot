package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

const sessionTitleTimeout = 5 * time.Second

// One driver per live slot coalesces title changes. Its wake-up queue is bounded
// and holds no transcript data; the authoritative target is read from SQLite.
type sessionTitleSync struct {
	wake   chan struct{}
	notice *sessionTitleNotice // guarded by slot.mu
}

type sessionTitleNotice struct {
	revision, desired string
	message           core.Message
}

// applySessionTitle only records and schedules metadata. Neither the first
// user input nor a chat command waits for the native pipe or control response.
func (g *Gateway) applySessionTitle(s *slot, worker core.Worker, first core.Message) {
	if worker == nil {
		return
	}
	s.mu.Lock()
	session := s.session.ID
	s.mu.Unlock()
	title, err := g.store.PrepareTitleSync(s.room.Key, session, nativeSessionTitle(first), true)
	if err != nil {
		g.audit(s.room.Key, "session_title_save_failed", core.Args{"error": err.Error()})
		return
	}
	if title.Desired != "" {
		g.queueSessionTitle(s, worker, title, nil)
	}
}

// renameNativeSession schedules the revision already committed by
// RenameSession. An inactive session keeps that intent for its next launch.
func (g *Gateway) renameNativeSession(r core.Room, session string, m core.Message) error {
	title, err := g.store.TitleSync(r.Key, session)
	if err != nil {
		return err
	}
	s := g.getSlot(r.Key)
	if s == nil || title.Desired == "" {
		return nil
	}
	s.mu.Lock()
	worker, current, live := s.worker, s.session.ID, s.live
	s.mu.Unlock()
	if worker != nil && live && current == session {
		g.queueSessionTitle(s, worker, title, &m)
	}
	return nil
}

func (g *Gateway) queueSessionTitle(s *slot, worker core.Worker, title store.SessionTitleSync, m *core.Message) {
	s.mu.Lock()
	if !s.live || s.worker != worker || s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	start := s.titleSync == nil
	if start {
		s.titleSync = &sessionTitleSync{wake: make(chan struct{}, 1)}
	}
	driver := s.titleSync
	if m != nil {
		driver.notice = &sessionTitleNotice{revision: title.Revision, desired: title.Desired, message: *m}
	} else if driver.notice != nil && driver.notice.desired == title.Desired {
		// A first-input launch may race an explicit /rename for this same
		// worker. Preserve its notification across the launch revision.
		driver.notice.revision = title.Revision
	}
	select {
	case driver.wake <- struct{}{}:
	default:
	}
	s.mu.Unlock()
	if start {
		g.task(func() { g.syncSessionTitles(s, worker, driver) })
	}
}

func (g *Gateway) syncSessionTitles(s *slot, worker core.Worker, driver *sessionTitleSync) {
	lastRevision := ""
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-driver.wake:
		}
		for s.ctx.Err() == nil {
			title, err := g.store.TitleSync(s.room.Key, s.session.ID)
			if err != nil {
				g.audit(s.room.Key, "session_title_read_failed", core.Args{"error": err.Error()})
				break
			}
			if title.State != "pending" || title.Desired == "" || title.Revision == lastRevision {
				// A /rename can attach its notice after this driver already
				// finished that revision; report the stored outcome once.
				if title.State != "pending" {
					g.sendSessionTitleNotice(s, driver, title.Revision, title.Desired, title.State)
				}
				break
			}
			lastRevision = title.Revision
			ctx, cancel := context.WithTimeout(s.ctx, sessionTitleTimeout)
			err = worker.Rename(ctx, title.Desired)
			cancel()
			state := "confirmed"
			if err != nil {
				state = string(core.ControlUnconfirmed)
				var failure *core.ControlFailure
				if errors.As(err, &failure) {
					state = string(failure.State)
				}
				g.audit(s.room.Key, "session_title_update_failed", core.Args{"revision": title.Revision, "state": state, "error": err.Error()})
			}
			applied, saveErr := g.store.FinishTitleSync(s.room.Key, s.session.ID, title.Revision, state)
			if saveErr != nil {
				g.audit(s.room.Key, "session_title_result_save_failed", core.Args{"revision": title.Revision, "error": saveErr.Error()})
			}
			if applied {
				g.sendSessionTitleNotice(s, driver, title.Revision, title.Desired, state)
			}
			// Re-read once to pick up a coalesced newer revision. The same
			// uncertain write is never retried without a new explicit intent.
		}
	}
}

// sendSessionTitleNotice delivers the pending /rename notice for revision at
// most once. Only completed native outcomes are reported.
func (g *Gateway) sendSessionTitleNotice(s *slot, driver *sessionTitleSync, revision, desired, state string) {
	switch state {
	case "confirmed", string(core.ControlNotSent), string(core.ControlRejected), string(core.ControlUnconfirmed):
	default:
		return
	}
	s.mu.Lock()
	notice := driver.notice
	if notice == nil || notice.revision != revision {
		notice = nil
	} else {
		driver.notice = nil
	}
	s.mu.Unlock()
	if notice != nil && s.ctx.Err() == nil {
		g.notify(s.room, sessionTitleResult(desired, state), &notice.message)
	}
}

func sessionTitleResult(title, state string) string {
	switch state {
	case "confirmed":
		return fmt.Sprintf("原生标题「%s」已确认同步。", title)
	case string(core.ControlRejected):
		return fmt.Sprintf("名称变更已保存；原生端拒绝了标题「%s」的同步，当前任务继续运行。", title)
	case string(core.ControlNotSent):
		return fmt.Sprintf("名称变更已保存；原生标题「%s」尚未发出，将在本会话下次启动时同步。", title)
	default:
		return fmt.Sprintf("名称变更已保存；原生标题「%s」的同步未收到确认，实际标题可能已改变。当前任务继续运行。", title)
	}
}
