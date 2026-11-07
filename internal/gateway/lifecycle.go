package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// A database failure must not abandon the half-stopped slot: it is already
	// cancelled and inactive, so returning early would leave a dead slot that
	// blocks this room and holds a worker slot. Finish the process stop and
	// report the state error afterwards.
	var stateErr error
	stopState := func() error {
		if e := g.store.StopSession(session); e != nil {
			return e
		}
		return g.cancelPendingOutputs(r, session)
	}
	if session != "" {
		g.cancelApprovals(session)
		g.cancelReceipts(session)
		if stateErr = stopState(); stateErr != nil {
			g.audit(r.Key, "stop_state_failed", core.Args{"session": session, "error": core.Redact(stateErr.Error(), g.secrets)})
			// Block at once: inputs arriving while the stop retries are told
			// the room is paused and that a successful stop cancels them,
			// instead of being cancelled without notice by a later retry.
			g.blockAfterFailedStop(r.Key)
			if s == nil {
				return errors.Join(errStopState, stateErr)
			}
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
	if stateErr != nil {
		// The process is stopped now; storage may have recovered meanwhile.
		for attempt := 0; stateErr != nil && attempt < len(stopStateRetry) && !g.isClosing(); attempt++ {
			time.Sleep(stopStateRetry[attempt])
			stateErr = stopState()
		}
	}
	if stateErr != nil {
		// Queued inputs could not be cancelled. Keep dispatch away from this
		// room until a later stop succeeds, so the stop's intent holds even
		// though sweeps and other rooms wake dispatch globally.
		g.blockAfterFailedStop(r.Key)
		return errors.Join(errStopState, stateErr)
	}
	held := 0
	if session != "" && g.stopIsBlocked(r.Key) {
		// Inputs held while blocked may have been queued after the cancel at
		// the start of this stop. Message counts and queues a held input
		// under g.mu, so cancelling and unblocking under g.mu here makes
		// every counted input cancelled and every later one ordinary.
		g.mu.Lock()
		e := g.store.StopSession(session)
		if e == nil {
			held = g.stopHeld[r.Key]
			delete(g.stopBlocked, r.Key)
			delete(g.stopHeld, r.Key)
		}
		g.mu.Unlock()
		if e != nil {
			g.audit(r.Key, "stop_state_failed", core.Args{"session": session, "error": core.Redact(e.Error(), g.secrets)})
			return errors.Join(errStopState, e)
		}
		g.cancelReceipts(session)
	} else {
		g.mu.Lock()
		delete(g.stopBlocked, r.Key)
		delete(g.stopHeld, r.Key)
		g.mu.Unlock()
	}
	if held > 0 {
		g.notify(r, "本聊天的停止已完成；停止期间暂存的输入已一并取消，如仍需要请重新发送。", nil)
	}
	g.kickWaiting()
	return nil
}

// errStopState marks a stop whose process part completed but whose state
// update (cancelling pending inputs or outputs) could not be saved.
var errStopState = errors.New("stop state could not be saved")

// stopStateRetry bounds retries of cancelling a stopped session's inputs.
var stopStateRetry = []time.Duration{50 * time.Millisecond, 300 * time.Millisecond, time.Second}

func (g *Gateway) blockAfterFailedStop(key string) {
	g.mu.Lock()
	g.stopBlocked[key] = true
	g.mu.Unlock()
}
func (g *Gateway) stopIsBlocked(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopBlocked[key]
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
	g.reconcileOrphanedInputs(ctx)
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
	g.mu.Unlock()
	// Scan rooms that actually own pending outputs. Topic rooms accumulate over
	// time, so retry work must not follow every room ever seen.
	keys, e := g.store.PendingOutputRooms(ctx)
	if e != nil {
		return
	}
	for _, key := range keys {
		r, ok := RoomFromKey(g.config, key)
		if !ok {
			continue
		}
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

// reconcileOrphanedInputs finds inputs still recorded as handed to a live
// process although that process no longer owns them, for example a turn whose
// final reply and input state both failed to save. They become uncertain
// (never replayed) so the room's process can be reclaimed again. The room's
// dispatch lock excludes the short window between marking an input sent and
// registering it with the process.
func (g *Gateway) reconcileOrphanedInputs(ctx context.Context) {
	for _, s := range g.allSlots() {
		s.mu.Lock()
		live := s.live && s.worker != nil
		key, session := s.room.Key, s.session.ID
		s.mu.Unlock()
		if !live || ctx.Err() != nil {
			continue
		}
		lockCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		unlock, err := g.dispatchLocks.lockContext(lockCtx, key)
		cancel()
		if err != nil {
			continue
		}
		rows, err := g.store.DB.QueryContext(ctx, "SELECT uuid,message FROM inbox WHERE room=? AND session=? AND status IN ('sent','consumed')", key, session)
		if err != nil {
			unlock()
			continue
		}
		type orphan struct {
			uuid string
			from core.Origin
		}
		var candidates []orphan
		for rows.Next() {
			var uuid, raw string
			if rows.Scan(&uuid, &raw) != nil {
				continue
			}
			var m core.Message
			_ = json.Unmarshal([]byte(raw), &m)
			candidates = append(candidates, orphan{uuid, core.Origin{ID: m.ID, SenderID: m.SenderID}})
		}
		rows.Close()
		s.mu.Lock()
		var orphans []orphan
		for _, c := range candidates {
			if _, owned := s.deliveries[c.uuid]; !owned && s.live {
				orphans = append(orphans, c)
			}
		}
		s.mu.Unlock()
		for _, o := range orphans {
			// Conditional: a result may have finished the input since it
			// was read; never overwrite done with uncertain.
			res, err := g.store.DB.ExecContext(ctx, "UPDATE inbox SET status='uncertain' WHERE uuid=? AND room=? AND status IN ('sent','consumed')", o.uuid, key)
			if n, _ := rowsAffected(res, err); n == 1 {
				g.audit(key, "orphaned_input_uncertain", core.Args{"session": session})
				if o.from.ID != "" {
					g.settleReceipts(key, session, []core.Origin{o.from}, false)
				}
			}
		}
		unlock()
	}
}

func rowsAffected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
