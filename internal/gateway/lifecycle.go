package gateway

import (
	"context"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"time"
)

func (g *Gateway) notify(r core.Room, text string, m *core.Message) {
	var copied *core.Message
	if m != nil {
		v := *m
		copied = &v
	}
	g.task(func() { _ = g.notice(g.ctx, r, text, copied) })
}
func (g *Gateway) notice(ctx context.Context, r core.Room, text string, m *core.Message) error {
	text = core.Redact(text, g.secrets)
	if g.isClosing() {
		return nil
	}
	sessions, e := g.store.ListSessions(r.Key)
	if e != nil {
		return e
	}
	session := ""
	for _, v := range sessions {
		if v.State == "open" {
			session = v.ID
			break
		}
	}
	reply, thread := "", ""
	if m != nil {
		reply = messageReply(*m)
		thread = m.ThreadID
	}
	id, e := provider(ctx, func() (string, error) { return g.transport.Send(ctx, r, text, reply, thread) })
	if g.isClosing() || ctx.Err() != nil {
		return ctx.Err()
	}
	if e != nil {
		g.audit(r.Key, "notice_failed", core.Args{})
		return e
	}
	if session != "" && id != "" {
		return g.store.RecordOutput(id, r.Key, session)
	}
	return nil
}
func (g *Gateway) stopRoom(ctx context.Context, r core.Room) error {
	g.clearImageTimer(r.Key)
	s := g.getSlot(r.Key)
	session := ""
	if s != nil {
		s.mu.Lock()
		if s.stopUnconfirmed && s.worker == nil {
			s.mu.Unlock()
			return errors.New("native worker termination remains unconfirmed")
		}
		s.live = false
		s.cancel()
		session = s.session.ID
		s.mu.Unlock()
		s.events.Lock()
	} else {
		sessions, e := g.store.ListSessions(r.Key)
		if e != nil {
			return e
		}
		for _, v := range sessions {
			if v.State == "open" {
				session = v.ID
				break
			}
		}
	}
	if session != "" {
		g.cancelApprovals(session)
		g.cancelReceipts(session)
		if e := g.store.StopSession(session); e != nil {
			if s != nil {
				s.events.Unlock()
			}
			return e
		}
		if e := g.cancelPendingOutputs(r, session); e != nil {
			if s != nil {
				s.events.Unlock()
			}
			return e
		}
	}
	if s != nil {
		s.events.Unlock()
	}
	unlock, lockErr := g.dispatchLocks.lockContext(ctx, r.Key)
	if lockErr != nil {
		if s != nil {
			s.mu.Lock()
			s.stopUnconfirmed = true
			s.mu.Unlock()
		}
		return lockErr
	}
	defer unlock()
	if s != nil {
		s.mu.Lock()
		worker := s.worker
		// Startup may have failed with unknown process ownership while
		// stop waited for dispatch. A nil handle cannot verify its cleanup.
		if worker == nil && s.stopUnconfirmed {
			s.mu.Unlock()
			return errors.New("native worker termination remains unconfirmed")
		}
		renders := make([]*render, 0, len(s.renders))
		for _, r := range s.renders {
			renders = append(renders, r)
		}
		s.mu.Unlock()
		var err error
		if worker != nil {
			err = providerVoid(ctx, func() error { return worker.Stop(ctx) })
		}
		for _, r := range renders {
			r.cancel("已停止")
		}
		if err != nil {
			s.mu.Lock()
			s.stopUnconfirmed = true
			s.mu.Unlock()
			g.audit(r.Key, "worker_stop_unconfirmed", core.Args{"session": session, "error": core.Redact(err.Error(), g.secrets)})
			return err
		}
		if err := g.waitRoomActions(ctx, r.Key); err != nil {
			s.mu.Lock()
			s.stopUnconfirmed = true
			s.mu.Unlock()
			return err
		}
		s.mu.Lock()
		s.stopUnconfirmed = false
		s.mu.Unlock()
		g.removeSlot(s)
	}
	if err := g.waitRoomActions(ctx, r.Key); err != nil {
		return err
	}
	g.kickWaiting()
	return nil
}
func (g *Gateway) reclaimable(s *slot) bool {
	s.mu.Lock()
	eligible := s.live && s.worker != nil && !s.busy && !s.pinned && s.background == 0 && s.activeRequests == 0 && len(s.deliveries) == 0 && !s.stopUnconfirmed
	s.mu.Unlock()
	if !eligible {
		return false
	}
	n, e := g.store.CountPending(s.room.Key)
	return e == nil && n == 0 && g.approvalCount(s.session.ID) == 0
}
func (g *Gateway) releaseIdle(ctx context.Context, s *slot) bool {
	// Recheck under the event lock before invalidating this native generation.
	s.events.Lock()
	if !g.reclaimable(s) {
		s.events.Unlock()
		return false
	}
	s.mu.Lock()
	s.live = false
	s.cancel()
	worker := s.worker
	s.mu.Unlock()
	s.events.Unlock()
	if e := providerVoid(ctx, func() error { return worker.Stop(ctx) }); e != nil {
		s.mu.Lock()
		s.stopUnconfirmed = true
		s.mu.Unlock()
		g.audit(s.room.Key, "idle_process_stop_unconfirmed", core.Args{"session": s.session.ID, "error": core.Redact(e.Error(), g.secrets)})
		g.notify(s.room, "闲置进程停止失败，尚未确认退出；已保留进程名额以避免并行执行。请检查服务诊断。", nil)
		return false
	}
	g.removeSlot(s)
	g.audit(s.room.Key, "idle_process_released", core.Args{"session": s.session.ID})
	return true
}

// Sweep reclaims confirmed idle native workers before retrying any network I/O.
// A stalled provider must not postpone reclamation of unrelated idle sessions.
func (g *Gateway) Sweep(ctx context.Context, now int64) {
	if g.isClosing() || !g.sweepMu.TryLock() {
		return
	}
	defer g.sweepMu.Unlock()
	g.poolMu.Lock()
	if g.config.Runtime.IdleMS > 0 {
		for _, s := range g.allSlots() {
			s.mu.Lock()
			last := s.lastActivity
			s.mu.Unlock()
			if now-last > g.config.Runtime.IdleMS && g.reclaimable(s) {
				g.releaseIdle(ctx, s)
			}
		}
	}
	g.poolMu.Unlock()
	g.kickWaiting()
	g.pruneExports(now)
	report, maintenanceErr := g.store.Maintain(ctx, now, g.config.Limits.BackgroundMS)
	if g.logger != nil {
		for i := range report.Policies {
			report.Policies[i].Error = core.Redact(report.Policies[i].Error, g.secrets)
		}
		detail := core.Args{"durationMs": report.DurationMS, "candidates": report.Candidates, "changed": report.Changed, "policies": report.Policies}
		if maintenanceErr != nil {
			detail["error"] = core.Redact(maintenanceErr.Error(), g.secrets)
		}
		g.logger("store_maintenance", detail)
	}
	g.retainCards(ctx, now)
	g.retryReceipts()
	g.mu.Lock()
	for key, values := range g.rates {
		alive := values[:0]
		for _, at := range values {
			if at > now-60000 {
				alive = append(alive, at)
			}
		}
		if len(alive) == 0 {
			delete(g.rates, key)
		} else {
			g.rates[key] = alive
		}
	}
	rooms := make([]core.Room, 0, len(g.rooms))
	for _, r := range g.rooms {
		rooms = append(rooms, r)
	}
	g.mu.Unlock()
	for _, r := range rooms {
		for _, id := range g.retryable(r.Key) {
			if ctx.Err() != nil {
				return
			}
			id := id
			g.task(func() { _, _ = g.deliverOutput(g.ctx, id, r, false) })
		}
	}
}
func (g *Gateway) Close(ctx context.Context) error {
	g.mu.Lock()
	already := g.closing
	g.closing = true
	g.cancel()
	for k, w := range g.imageTimers {
		w.timer.Stop()
		delete(g.imageTimers, k)
	}
	slots := make([]*slot, 0, len(g.slots))
	for _, s := range g.slots {
		slots = append(slots, s)
	}
	g.mu.Unlock()
	if !already {
		g.closeApprovals()
		for _, s := range slots {
			s.mu.Lock()
			s.live = false
			s.cancel()
			s.mu.Unlock()
		}
		for _, s := range slots {
			unlock, err := g.dispatchLocks.lockContext(ctx, s.room.Key)
			if err != nil {
				s.mu.Lock()
				s.stopUnconfirmed = true
				s.mu.Unlock()
				return err
			}
			s.events.Lock()
			g.cancelReceipts(s.session.ID)
			s.mu.Lock()
			worker := s.worker
			renders := make([]*render, 0, len(s.renders))
			for _, r := range s.renders {
				renders = append(renders, r)
			}
			s.mu.Unlock()
			s.events.Unlock()
			if worker != nil {
				err := providerVoid(ctx, func() error { return worker.Stop(ctx) })
				s.mu.Lock()
				s.stopUnconfirmed = err != nil
				s.mu.Unlock()
				if err != nil {
					g.audit(s.room.Key, "worker_stop_unconfirmed", core.Args{"session": s.session.ID, "error": core.Redact(err.Error(), g.secrets)})
				}
			}
			for _, r := range renders {
				r.cancel("已停止")
			}
			unlock()
		}
	}
	drained := make(chan struct{})
	go func() { g.wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-ctx.Done():
		return ctx.Err()
	}
	unconfirmed := 0
	for _, s := range g.allSlots() {
		s.mu.Lock()
		bad := s.stopUnconfirmed
		s.mu.Unlock()
		if bad {
			unconfirmed++
		} else {
			g.removeSlot(s)
		}
	}
	g.mu.Lock()
	unsafeActions := len(g.actions)
	g.mu.Unlock()
	if unsafeActions > 0 {
		return fmt.Errorf("project action process termination unconfirmed (%d); service lock must remain held", unsafeActions)
	}
	if unconfirmed > 0 {
		return fmt.Errorf("native worker termination unconfirmed (%d); verify native processes before restarting", unconfirmed)
	}
	return nil
}
func (g *Gateway) beginCall() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return false
	}
	g.wg.Add(1)
	return true
}

// withTimeout is used only for nonessential cosmetic updates after cancellation.
func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
