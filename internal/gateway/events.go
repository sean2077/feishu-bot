package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nativeIDPattern = regexp.MustCompile(`(?i)^[a-f0-9]{8}-(?:[a-f0-9]{4}-){3}[a-f0-9]{12}$`)

func backgroundRenderKey(uuid, id string) string {
	return "background:" + jsonText([]any{nullString(uuid), nullString(id)})
}
func (g *Gateway) renderFor(s *slot, uuid, source, backgroundID string) *render {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, human := s.deliveries[uuid]
	human = source != "background" && uuid != "" && human
	var anchor *core.Delivery
	key := "unbound:foreground"
	if human {
		key = "human"
		a, ok := s.deliveries[s.anchor]
		if !ok {
			a = s.deliveries[uuid]
		}
		anchor = &a
	} else {
		if uuid != "" || source == "background" {
			key = backgroundRenderKey(uuid, backgroundID)
		}
		if a, ok := s.replyAnchors[uuid]; ok {
			anchor = &a
		}
	}
	r := s.renders[key]
	if r == nil {
		r = newRender(g, s, anchor)
		s.renders[key] = r
	}
	if s.turnRenders == nil {
		s.turnRenders = map[string]bool{}
	}
	s.turnRenders[key] = true
	return r
}
func (g *Gateway) event(s *slot, ev core.WorkerEvent) {
	s.events.Lock()
	defer s.events.Unlock()
	if !g.active(s) {
		if ev.Type == "ready" {
			// A stop may land between the first input and the native init.
			// The context still exists, so its ID must be kept even though
			// this process no longer accepts work.
			if nativeIDPattern.MatchString(ev.CCID) && !g.keepNativeIdentity(s, ev.CCID) {
				g.notify(s.room, identityFailureNotice, nil)
			}
		}
		return
	}
	s.mu.Lock()
	s.lastActivity = time.Now().UnixMilli()
	s.mu.Unlock()
	switch ev.Type {
	case "ready":
		if !nativeIDPattern.MatchString(ev.CCID) {
			return
		}
		s.mu.Lock()
		old := s.session.CCID
		s.mu.Unlock()
		if old != "" && old != ev.CCID {
			s.mu.Lock()
			s.live = false
			s.mu.Unlock()
			g.task(func() {
				_ = g.stopGeneration(g.ctx, s)
				g.notify(s.room, "原会话恢复返回了不同 ID，已停止，未重置历史。请检查原生会话文件。", nil)
			})
			return
		}
		if old == "" {
			if !g.keepNativeIdentity(s, ev.CCID) {
				// The ID is neither in the database nor in the workspace
				// marker. Continuing would let the next launch silently start a
				// different context, so stop instead of accepting more work.
				s.mu.Lock()
				s.live = false
				s.mu.Unlock()
				g.task(func() {
					_ = g.stopGeneration(g.ctx, s)
					g.notify(s.room, identityFailureNotice, nil)
				})
				return
			}
		} else if e := g.store.SetCC(s.session.ID, ev.CCID); e != nil {
			g.audit(s.room.Key, "session_identity_persistence_failed", core.Args{"error": e.Error()})
			return
		}
		s.mu.Lock()
		if ev.StartupMS > 0 {
			s.startupMs = ev.StartupMS
		}
		if s.turn != nil && !s.turn.reused {
			s.turn.startupMs = ev.StartupMS
		}
		s.session.CCID = ev.CCID
		if ev.PermissionMode != "" {
			s.effectivePermissionMode = ev.PermissionMode
		}
		s.mu.Unlock()
		if ev.PermissionMode != "" {
			project, _ := g.config.Project(s.room.ProjectID)
			requested := project.PermissionMode
			if requested == "" {
				requested = "acceptEdits"
			}
			// Claude Code 2.1.321 echoes default, acceptEdits, bypassPermissions
			// and auto verbatim; report any future mismatch instead of mapping it.
			if ev.PermissionMode != requested {
				g.audit(s.room.Key, "permission_mode_mismatch", core.Args{"requested": requested, "effective": ev.PermissionMode})
			}
		}
	case "accepted":
		s.mu.Lock()
		d, ok := s.deliveries[ev.UUID]
		accepted := s.accepted[ev.UUID]
		if s.turn != nil && s.turn.acceptedAt.IsZero() {
			s.turn.acceptedAt = time.Now()
		}
		if ok && d.SupplementTo != "" && !accepted {
			s.accepted[ev.UUID] = true
		}
		s.mu.Unlock()
		if ok && d.SupplementTo != "" && !accepted {
			r, allowed := routeQueued(g.config, s.room.Key, d.Message)
			if allowed {
				if receipt := supplementReceipt(d.Message); receipt != "" {
					g.notify(r, receipt, &d.Message)
				}
			}
		}
	case "input_rejected":
		s.mu.Lock()
		d, ok := s.deliveries[ev.UUID]
		accepted := s.accepted[ev.UUID]
		s.mu.Unlock()
		if ok && d.SupplementTo != "" && !accepted && ev.Reason == "foreground_ended" && g.cancelSupplement(s.room, d, "原生接收补充前，目标前台运行已结束。", true, "sent") {
			s.mu.Lock()
			delete(s.deliveries, ev.UUID)
			delete(s.replyAnchors, ev.UUID)
			s.advanceAnchorLocked()
			s.busy = len(s.deliveries) > 0
			s.mu.Unlock()
			g.kickWaiting()
		}
	case "consumed":
		s.mu.Lock()
		ids := append([]string(nil), s.inputGroups[ev.UUID]...)
		if len(ids) == 0 {
			ids = []string{ev.UUID}
		}
		valid := []string{}
		for _, id := range ids {
			if _, ok := s.deliveries[id]; ok {
				valid = append(valid, id)
			}
		}
		s.mu.Unlock()
		if err := g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
			for _, id := range valid {
				if _, e := tx.Exec("UPDATE inbox SET status='consumed' WHERE uuid=? AND room=? AND session=? AND status='sent'", id, s.room.Key, s.session.ID); e != nil {
					return e
				}
				if e := g.store.ConfirmTopicRoot(tx, s.room.Key, s.session.ID, id); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			g.audit(s.room.Key, "input_consumption_persistence_failed", core.Args{"error": err.Error()})
		}
	case "activity":
		s.mu.Lock()
		if s.turn != nil {
			switch ev.Phase {
			case "tool_start":
				if ev.ToolUseID != "" {
					s.turn.toolStarts[ev.ToolUseID] = time.Now()
					s.turn.toolNames[ev.ToolUseID] = ev.Tool
				}
			case "tool_end":
				if start, ok := s.turn.toolStarts[ev.ToolUseID]; ok {
					s.turn.tools += time.Since(start)
					delete(s.turn.toolStarts, ev.ToolUseID)
				}
			}
		}
		if ev.Background != nil {
			s.background = *ev.Background
		}
		if ev.Pinned != nil {
			s.pinned = *ev.Pinned
		}
		if ev.State != "" {
			s.busy = ev.State != "idle"
		}
		if strings.Contains(ev.Tool, "Monitor") || strings.Contains(ev.Tool, "CronCreate") || strings.Contains(ev.Tool, "ScheduleWakeup") {
			s.pinned = true
		}
		s.mu.Unlock()
		if ev.State == "idle" {
			g.cancelLostSupplements(s.room)
		}
		g.kickWaiting()
	case "auto_denied":
		s.mu.Lock()
		tool, session, anchor := "", s.session.ID, s.anchor
		if s.turn != nil {
			tool = s.turn.toolNames[ev.ToolUseID]
		}
		s.mu.Unlock()
		reason := core.Redact(ev.Text, g.secrets)
		if ev.Phase == "classifier" {
			g.audit(s.room.Key, "auto_mode_denied", core.Args{"session": session, "tool": tool, "reason": reason})
		} else {
			kind := ev.Phase
			if kind != "unavailable" {
				kind = "unevaluable"
			}
			g.audit(s.room.Key, "auto_mode_unavailable", core.Args{"session": session, "tool": tool, "kind": kind, "reason": reason})
		}
		if anchor != "" {
			label := tool
			if label == "" {
				label = "工具"
			}
			notice := fmt.Sprintf("自动模式安全审查不可用，%s 未执行；请联系维护者。", label)
			if ev.Phase == "classifier" {
				notice = fmt.Sprintf("安全分类器拦截了 %s，操作未执行；如需执行请联系维护者。", label)
			}
			g.renderFor(s, anchor, "foreground", "").setNotice(notice)
		}
	case "text", "thinking":
		s.mu.Lock()
		if s.turn != nil && s.turn.firstAt.IsZero() {
			s.turn.firstAt = time.Now()
		}
		s.mu.Unlock()
		g.renderFor(s, ev.TurnID, ev.Source, ev.BackgroundID).update(core.Redact(ev.Text, g.secrets), ev.Partial, ev.Type, ev.BlockID)
	case "compaction":
		s.mu.Lock()
		id := s.compacting
		if id == "" {
			id = s.anchor
		}
		s.mu.Unlock()
		text := "上下文已整理"
		if ev.Phase == "start" {
			text = "正在整理原生上下文"
		}
		g.renderFor(s, id, "", "").setStatus(text)
	case "result":
		g.resultEvent(s, ev)
	case "request":
		s.mu.Lock()
		known := s.requests[ev.ID]
		exhausted := len(s.requests) >= 10000
		worker := s.worker
		if !known && !exhausted {
			s.requests[ev.ID] = true
			s.activeRequests++
		}
		s.mu.Unlock()
		if exhausted {
			if worker != nil {
				g.task(func() {
					_ = worker.Respond(s.ctx, ev.ID, core.Args{"approved": false, "error": "session request budget exceeded"})
				})
			}
			g.notify(s.room, "本会话请求数达到安全上限，请在任务结束后新建会话。", nil)
			return
		}
		if known {
			return
		}
		g.task(func() {
			defer func() { s.mu.Lock(); s.activeRequests--; s.mu.Unlock(); g.kickWaiting() }()
			// Native initialization hooks can emit controls before the factory
			// hands back its handle. Do not lose an early permission response.
			select {
			case <-s.workerReady:
			case <-s.ctx.Done():
				return
			}
			answer, e := g.request(s, ev)
			if e != nil {
				answer = core.Args{"approved": false, "ok": false, "error": core.Redact(e.Error(), g.secrets)}
			}
			s.mu.Lock()
			worker, live := s.worker, s.live
			s.mu.Unlock()
			if live && worker != nil {
				if e = worker.Respond(s.ctx, ev.ID, answer); e != nil {
					g.audit(s.room.Key, "native_response_failed", core.Args{"error": core.Redact(e.Error(), g.secrets)})
				}
			}
		})
	case "error":
		s.mu.Lock()
		d, ok := s.deliveries[s.anchor]
		s.mu.Unlock()
		g.audit(s.room.Key, "worker_failure", core.Args{"session": s.session.ID, "phase": "initialize", "reason": workerFailureReason(ev.Message), "error": core.Redact(ev.Message, g.secrets)})
		var m *core.Message
		if ok {
			m = &d.Message
		}
		g.notify(s.room, core.Redact(ev.Message, g.secrets), m)
	case "exit":
		g.exitEvent(s, ev)
	}
}
func (s *slot) advanceAnchorLocked() {
	out := s.order[:0]
	for _, id := range s.order {
		if _, ok := s.deliveries[id]; ok {
			out = append(out, id)
		}
	}
	s.order = out
	s.anchor = ""
	if len(out) > 0 {
		s.anchor = out[0]
	}
}
func (g *Gateway) resultEvent(s *slot, ev core.WorkerEvent) {
	disposition := ""
	// A bookkeeping retry must not insert usage_results between staging and
	// the first native result's deduplication check, or that first answer
	// would be mistaken for a duplicate and discarded.
	finishUsage := g.stageResultUsage(s, ev)
	defer finishUsage()
	record := func() error {
		return g.store.Transaction(context.Background(), func(tx *sql.Tx) error { var e error; disposition, e = g.recordUsage(tx, s, ev); return e })
	}
	err := record()
	for attempt := 0; err != nil && attempt < len(finalPersistRetry) && !g.isClosing(); attempt++ {
		time.Sleep(finalPersistRetry[attempt])
		err = record()
	}
	if err != nil {
		// Usage is bookkeeping. The native process will not repeat this
		// result, so dropping it here would leave the turn busy forever; keep
		// handling the reply while the pending observation makes /usage's
		// incomplete coverage explicit and permits a later accounting retry.
		g.audit(s.room.Key, "usage_persistence_failed", core.Args{"session": s.session.ID, "error": err.Error()})
		disposition = "unrecorded"
	}
	if disposition == "duplicate" {
		return
	}
	s.mu.Lock()
	var turn *turnTiming
	if ev.Source != "background" {
		turn = s.turn
		s.turn = nil
	}
	worker := s.worker
	s.mu.Unlock()
	if ev.Source != "background" {
		setWorkerTurn(worker, false)
	}
	turnArgs := core.Args{"session": s.session.ID, "success": ev.Success, "compact": ev.Compact, "resultId": ev.ResultID, "models": ev.Models, "usage": ev.Usage}
	for key, value := range turnResultTiming(turn) {
		turnArgs[key] = value
	}
	g.audit(s.room.Key, "turn_result", turnArgs)
	nativeIDs := ev.UUIDs
	if len(nativeIDs) == 0 && ev.UUID != "" {
		nativeIDs = []string{ev.UUID}
	}
	s.mu.Lock()
	finished := []string{}
	for _, native := range nativeIDs {
		ids := s.inputGroups[native]
		if len(ids) == 0 {
			ids = []string{native}
		}
		for _, id := range ids {
			if _, ok := s.deliveries[id]; ok && !contains(finished, id) {
				finished = append(finished, id)
			}
		}
	}
	var anchor *core.Delivery
	if len(finished) > 0 {
		v := s.deliveries[finished[0]]
		anchor = &v
	} else if v, ok := s.replyAnchors[ev.ReplyToUUID]; ok {
		anchor = &v
	}
	_, stillOwned := s.deliveries[ev.ReplyToUUID]
	ownsReceipt := !(ev.Source == "background" && ev.ReplyToUUID != "" && stillOwned)
	who := []core.Origin{}
	for _, id := range finished {
		d := s.deliveries[id]
		who = append(who, core.Origin{ID: d.Message.ID, SenderID: d.Message.SenderID})
	}
	if len(who) == 0 && anchor != nil && ownsReceipt {
		who = origin(anchor.Message)
	}
	key := "unbound:foreground"
	if len(finished) > 0 {
		key = "human"
	} else if ev.ReplyToUUID != "" || ev.Source == "background" {
		key = backgroundRenderKey(ev.ReplyToUUID, ev.BackgroundID)
	}
	r := s.renders[key]
	folded := false
	var excerpt *render
	if len(finished) > 0 && ev.Source == "background" && ev.BackgroundID != "" && ev.ReplyToUUID != "" {
		bk := backgroundRenderKey(ev.ReplyToUUID, ev.BackgroundID)
		if p := s.renders[bk]; p != nil {
			delete(s.renders, bk)
			if r == nil {
				r = p
				folded = true
			} else {
				excerpt = p
			}
		}
	}
	if r == nil && ev.Source != "background" {
		r = s.renders["unbound:foreground"]
		if r != nil {
			delete(s.renders, "unbound:foreground")
			r.bind(anchor)
		}
	}
	s.mu.Unlock()
	if r == nil {
		uuid := ev.ReplyToUUID
		source := ev.Source
		if len(finished) > 0 {
			uuid = finished[0]
			source = "foreground"
		}
		r = g.renderFor(s, uuid, source, ev.BackgroundID)
	}
	// The native turn ends here. Another render that received output in it,
	// e.g. a background card opened by a task notification the native turn
	// absorbed, would otherwise keep streaming forever.
	s.mu.Lock()
	var orphans []*render
	for k := range s.turnRenders {
		if p := s.renders[k]; p != nil && p != r && p != excerpt {
			delete(s.renders, k)
			orphans = append(orphans, p)
		}
	}
	s.turnRenders = nil
	s.mu.Unlock()
	model := ev.Model
	if model == "" {
		model = strings.Join(ev.Models, " / ")
	}
	r.setDetails(replyDetails{Model: model, ProjectPath: g.workspaceLabel(s.room, s.room.ProjectID, s.workspace.Work), Context: ev.Context, Usage: ev.Usage})
	answer := ev.Text
	compact := map[string]string{"compacted": "上下文已由 Claude Code 原生压缩，会话保持不变。", "not_needed": "原生压缩未执行（例如历史不足），会话保持不变。", "failed": "原生压缩失败，未新建或重置会话。"}[ev.Compact]
	if compact != "" {
		answer = compact
		if ev.Text != "" {
			answer += "\n\n" + ev.Text
		}
	}
	if folded && ev.ReplyToUUID != "" && !contains(finished, ev.ReplyToUUID) {
		answer = "本轮后台工作已处理后续消息；卡片仍引用最初触发后台工作的消息。\n\n" + answer
	}
	title := ""
	if ev.Compact != "" {
		title = "上下文整理"
	}
	r.mu.Lock()
	r.detached = len(finished) == 0 && ev.Source == "background"
	r.mu.Unlock()
	persist := func() (func(), error) {
		return r.persistWith(answer, ev.Success, who, title, finished, func(tx *sql.Tx) error {
			if disposition == "unrecorded" {
				return g.stageUsage(tx, s, ev)
			}
			return nil
		})
	}
	drain, err := persist()
	for attempt := 0; err != nil && attempt < len(finalPersistRetry) && !g.isClosing(); attempt++ {
		// Usage is already recorded, so a duplicate result cannot retry this.
		// A transient database failure gets bounded retries here instead.
		time.Sleep(finalPersistRetry[attempt])
		drain, err = persist()
	}
	if err != nil {
		g.audit(s.room.Key, "final_reply_persistence_failed", core.Args{"session": s.session.ID, "error": err.Error()})
		g.abandonResult(s, key, r, finished, nativeIDs, who)
		for _, p := range append(orphans, excerpt) {
			if p != nil {
				// Already detached from s.renders; nothing else would close it.
				g.task(func() { p.cancel("后台续轮已结束") })
			}
		}
		g.notify(s.room, "回复保存失败，本轮不标记完成；未确认输入不会自动重放。请检查服务诊断和磁盘状态。", nil)
		return
	}
	r.mu.Lock()
	outputID := r.finalID
	r.mu.Unlock()
	if turn != nil && outputID != "" {
		g.rememberTurnDelivery(outputID, &turnDeliveryReport{turn: turn.turn, session: s.session.ID, startedAt: turn.queuedAt})
	}
	s.mu.Lock()
	delete(s.renders, key)
	for _, id := range finished {
		delete(s.deliveries, id)
		delete(s.accepted, id)
	}
	for _, id := range nativeIDs {
		delete(s.inputGroups, id)
	}
	if contains(finished, s.compacting) {
		s.compacting = ""
	}
	s.busy = len(s.deliveries) > 0
	s.advanceAnchorLocked()
	s.activeRequests++
	s.mu.Unlock()
	g.settleReceipts(s.room.Key, s.session.ID, who, false)
	g.cancelLostSupplements(s.room)
	if !g.task(func() { defer func() { s.mu.Lock(); s.activeRequests--; s.mu.Unlock(); g.kickWaiting() }(); drain() }) {
		s.mu.Lock()
		s.activeRequests--
		s.mu.Unlock()
		drain()
	}
	if excerpt != nil {
		g.finishFragment(s, excerpt)
	}
	for _, p := range orphans {
		g.finishFragment(s, p)
	}
	g.kickWaiting()
}

// finishFragment finalizes a detached render whose native turn was answered
// on another reply card. A render that never reached the platform is only
// closed, so ending it does not post a new card.
func (g *Gateway) finishFragment(s *slot, p *render) {
	p.mu.Lock()
	visible := p.messageID != "" || p.flushing || p.createUncertain || p.initialCard != nil
	p.mu.Unlock()
	if !visible {
		p.cancel("后台续轮已结束")
		return
	}
	finish, e := p.persist("本轮后台片段已结束；后续消息的答复见本轮另一张回复卡片。", true, nil, "后台续轮已结束", nil)
	if e != nil {
		g.audit(s.room.Key, "final_reply_persistence_failed", core.Args{"session": s.session.ID, "error": e.Error()})
		g.task(func() { p.cancel("后台续轮已结束") })
		return
	}
	s.mu.Lock()
	s.activeRequests++
	s.mu.Unlock()
	if !g.task(func() { defer func() { s.mu.Lock(); s.activeRequests--; s.mu.Unlock(); g.kickWaiting() }(); finish() }) {
		s.mu.Lock()
		s.activeRequests--
		s.mu.Unlock()
		finish()
	}
}
func (g *Gateway) exitEvent(s *slot, ev core.WorkerEvent) {
	cleanupFailed := ev.CleanupConfirmed != nil && !*ev.CleanupConfirmed
	s.mu.Lock()
	interrupted := len(s.deliveries) > 0 || s.background > 0 || s.pinned
	s.live = false
	s.stopUnconfirmed = cleanupFailed
	s.cancel()
	renders := []*render{}
	for _, r := range s.renders {
		renders = append(renders, r)
	}
	s.mu.Unlock()
	approvals := g.approvalCount(s.session.ID)
	g.cancelApprovals(s.session.ID)
	g.cancelReceipts(s.session.ID)
	notice, err := g.persistExited(context.Background(), s, cleanupFailed, interrupted, approvals)
	s.mu.Lock()
	s.exitPending, s.exitInterrupted, s.exitApprovals = err != nil, interrupted, approvals
	s.mu.Unlock()
	if err != nil {
		g.audit(s.room.Key, "worker_exit_persistence_failed", core.Args{"error": err.Error()})
	}
	if cleanupFailed {
		g.audit(s.room.Key, "worker_descendants_unconfirmed", core.Args{"session": s.session.ID})
	}
	g.cancelLostSupplements(s.room)
	for _, r := range renders {
		r := r
		g.task(func() { r.cancel("执行中断") })
	}
	if notice != "" {
		g.task(func() { _, _ = g.deliverOutput(g.ctx, notice, s.room, false) })
	}
	if !cleanupFailed {
		// Durable queued inputs, not an in-memory room history, decide which
		// rooms may now use the released capacity.
		keys, _ := g.store.PendingRooms()
		for _, key := range keys {
			candidate, ok := RoomFromKey(g.config, key)
			if !ok {
				continue
			}
			if candidate.Key != s.room.Key {
				g.kick(candidate)
			} else {
				pending, _ := g.store.Pending(candidate.Key)
				for _, d := range pending {
					if d.ImageBatch != nil {
						g.kick(candidate)
						break
					}
				}
			}
		}
	}
	if !cleanupFailed && err == nil {
		// Keep this generation discoverable until its last state access is
		// complete. Close either sees this slot and drains its callbacks, or
		// sees it already removed after all callback-owned persistence.
		g.releaseExited(s)
	}
}

func (g *Gateway) persistExited(ctx context.Context, s *slot, cleanupFailed, interrupted bool, approvals int) (string, error) {
	notice := ""
	s.mu.Lock()
	created := s.handedOff || s.identityObserved
	s.mu.Unlock()
	err := g.store.Transaction(ctx, func(tx *sql.Tx) error {
		if !cleanupFailed && !created {
			// The process exited on its own before any input was handed off
			// and is reaped, so no native context can exist or appear later.
			// Keeping the launch fence would refuse this session permanently.
			if _, e := tx.ExecContext(ctx, "DELETE FROM native_identity_pending WHERE session=?", s.session.ID); e != nil {
				return e
			}
		}
		res, e := tx.ExecContext(ctx, "UPDATE inbox SET status='uncertain' WHERE room=? AND session=? AND status IN ('sent','consumed')", s.room.Key, s.session.ID)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if cleanupFailed || interrupted || approvals > 0 || n > 0 {
			text := "原生工作进程意外退出。当前审批／追问已取消；未确认的输入与外部操作不会自动重放。请先核实结果。会话记录仍保留，可以继续原会话。"
			if cleanupFailed {
				text = "原生进程异常退出，但后代清理未确认。本聊天已暂停；不会启动替代进程或重放未确认操作。请维护者核实进程状态。"
			}
			notice, e = g.saveOutput(tx, s.room, s.session.ID, text, false, "", "", "", DisplayCard("工作中断", text), nil, "", "", "recovery", "")
			return e
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return notice, nil
}

var _ = fmt.Sprint

// identityFailureNotice explains a native session whose ID could not be kept.
const identityFailureNotice = "原生会话 ID 未能保存或与已记录的会话冲突，已停止本聊天运行，未启动替代上下文。请检查服务诊断；维护者可用 /resume 选择已记录的会话或用 /new 明确开启新会话。"

// identityPersistRetry bounds retries of saving a new native session ID.
var identityPersistRetry = []time.Duration{20 * time.Millisecond, 200 * time.Millisecond, time.Second}

// finalPersistRetry bounds synchronous retries of a final reply whose database
// transaction failed. The event reader is blocked meanwhile, so keep it short.
var finalPersistRetry = []time.Duration{100 * time.Millisecond, 400 * time.Millisecond, time.Second}

// abandonResult releases a turn whose final reply could not be saved. The
// render and deliveries must not stay attached to the slot: a closed render
// would otherwise swallow every later reply in this room, and the deliveries
// would keep the process busy forever. The model did process the inputs, so
// they become uncertain rather than cancelled, and are never replayed.
func (g *Gateway) abandonResult(s *slot, key string, r *render, finished, nativeIDs []string, who []core.Origin) {
	s.mu.Lock()
	if s.renders[key] == r {
		delete(s.renders, key)
	}
	for _, id := range finished {
		delete(s.deliveries, id)
		delete(s.accepted, id)
	}
	for _, id := range nativeIDs {
		delete(s.inputGroups, id)
	}
	if contains(finished, s.compacting) {
		s.compacting = ""
	}
	s.busy = len(s.deliveries) > 0
	s.advanceAnchorLocked()
	s.mu.Unlock()
	for _, id := range finished {
		if e := g.store.Mark(id, "uncertain"); e != nil {
			g.audit(s.room.Key, "input_state_unconfirmed", core.Args{"session": s.session.ID})
		}
	}
	g.settleReceipts(s.room.Key, s.session.ID, who, false)
	g.task(func() { r.cancel("回复保存失败") })
	g.kickWaiting()
}

// keepNativeIdentity durably records a newly reported native session ID: in
// the database, or, if that keeps failing, in a marker file in the room's
// workspace that the next launch adopts before starting. It reports whether
// either record exists, i.e. whether the native context stays resumable.
func (g *Gateway) keepNativeIdentity(s *slot, ccid string) bool {
	if !nativeIDPattern.MatchString(ccid) {
		return false
	}
	s.mu.Lock()
	s.identityObserved = true
	old, session, root := s.session.CCID, s.session.ID, s.workspace.Root
	s.mu.Unlock()
	if old != "" {
		return old == ccid
	}
	e := g.store.SetCC(session, ccid)
	for attempt := 0; e != nil && !errors.Is(e, store.ErrIdentityConflict) && attempt < len(identityPersistRetry); attempt++ {
		time.Sleep(identityPersistRetry[attempt])
		e = g.store.SetCC(session, ccid)
	}
	if e == nil {
		removeIdentityMarker(root, session)
		return true
	}
	// The native ID is recorded so a maintainer can recover the context by
	// hand if neither the database nor the marker keeps it.
	g.audit(s.room.Key, "session_identity_persistence_failed", core.Args{"session": session, "nativeSessionId": ccid, "error": e.Error()})
	if errors.Is(e, store.ErrIdentityConflict) {
		// The row already names another native context (for example a ready
		// event from a stopped process raced a fresh launch). A marker would
		// hide the conflict; report it instead.
		return false
	}
	if root != "" && writeIdentityMarker(root, session, ccid) == nil {
		g.audit(s.room.Key, "session_identity_marked", core.Args{"session": session})
		return true
	}
	// Neither record exists: never launch this session again without an
	// explicit /new or /resume, which select a different session.
	g.mu.Lock()
	g.identityLost[session] = true
	g.mu.Unlock()
	return false
}

func (g *Gateway) sessionIdentityLost(session string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.identityLost[session]
}

func identityMarkerPath(root, session string) string {
	return filepath.Join(root, "native-session-"+core.Hash(session)[:24])
}

// writeIdentityMarker atomically records session→native ID beside the
// workspace, outside the database whose write just failed.
func writeIdentityMarker(root, session, ccid string) error {
	return syncStateMarker(identityMarkerPath(root, session), []byte(session+"\n"+ccid+"\n"))
}

// readIdentityMarker returns the kept native ID of session. A missing marker
// is ("", false, nil); any other unreadable or malformed marker is an error,
// because treating it as absent would launch a fresh native context.
func readIdentityMarker(root, session string) (string, bool, error) {
	if root == "" {
		return "", false, nil
	}
	data, err := os.ReadFile(identityMarkerPath(root, session))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil || len(data) > 512 {
		return "", false, errors.New("native session marker is unreadable")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != session || !nativeIDPattern.MatchString(strings.TrimSpace(lines[1])) {
		return "", false, errors.New("native session marker is malformed")
	}
	return strings.TrimSpace(lines[1]), true, nil
}

func removeIdentityMarker(root, session string) {
	if root != "" {
		_ = os.Remove(identityMarkerPath(root, session))
	}
}
