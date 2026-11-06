package gateway

import (
	"context"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const sessionTitleTimeout = 5 * time.Second

// applySessionTitle keeps the Claude Code session list readable. A name set by
// /rename always wins, then the title remembered from the session's first
// admitted message. Only when neither exists does it derive and remember one;
// later messages never rename an existing session.
func (g *Gateway) applySessionTitle(s *slot, worker core.Worker, first core.Message) {
	if worker == nil {
		return
	}
	s.mu.Lock()
	session := s.session.ID
	s.mu.Unlock()
	p, err := g.store.Preferences(s.room.Key, session)
	if err != nil {
		g.audit(s.room.Key, "session_title_read_failed", core.Args{"error": err.Error()})
		return
	}
	title := p.Name
	if title == "" {
		title = p.NativeTitle
	}
	if title == "" {
		title = nativeSessionTitle(first)
		if title != "" {
			if err := g.store.SetNativeTitle(s.room.Key, session, title); err != nil {
				g.audit(s.room.Key, "session_title_save_failed", core.Args{"error": err.Error()})
			}
		}
	}
	if title == "" {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, sessionTitleTimeout)
	defer cancel()
	if err := worker.Rename(ctx, title); err != nil {
		g.audit(s.room.Key, "session_title_update_failed", core.Args{"error": err.Error()})
	}
}

// renameNativeSession applies a /rename to the live native process. Clearing
// the chat name restores the remembered automatic title when one exists.
func (g *Gateway) renameNativeSession(r core.Room, session, name string) {
	if name == "" {
		p, err := g.store.Preferences(r.Key, session)
		if err != nil {
			g.audit(r.Key, "session_title_read_failed", core.Args{"error": err.Error()})
			return
		}
		name = p.NativeTitle
	}
	if name == "" {
		return
	}
	s := g.getSlot(r.Key)
	if s == nil {
		return
	}
	s.mu.Lock()
	worker, current, live := s.worker, s.session.ID, s.live
	s.mu.Unlock()
	if worker == nil || !live || current != session {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, sessionTitleTimeout)
	defer cancel()
	if err := worker.Rename(ctx, name); err != nil {
		g.audit(r.Key, "session_title_update_failed", core.Args{"error": err.Error()})
	}
}
