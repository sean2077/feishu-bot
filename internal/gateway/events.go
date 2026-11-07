package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
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
	return r
}
func (g *Gateway) event(s *slot, ev core.WorkerEvent) {
	s.events.Lock()
	defer s.events.Unlock()
	if !g.active(s) {
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
		s.nativeReady = true
		s.mu.Unlock()
		if old != "" && old != ev.CCID {
			g.task(func() {
				_ = g.stopRoom(g.ctx, s.room)
				g.notify(s.room, "原会话恢复返回了不同 ID，已停止，未重置历史。请检查原生会话文件。", nil)
			})
			return
		}
		e := g.store.SetCC(s.session.ID, ev.CCID)
		for attempt := 0; e != nil && old == "" && attempt < len(identityPersistRetry); attempt++ {
			time.Sleep(identityPersistRetry[attempt])
			e = g.store.SetCC(s.session.ID, ev.CCID)
		}
		if e != nil {
			g.audit(s.room.Key, "session_identity_persistence_failed", core.Args{"error": e.Error()})
			if old == "" {
				// A native context whose ID is not durable cannot be resumed
				// later; continuing would make the next launch silently start a
				// different context. Stop instead of accepting more work.
				g.task(func() {
					_ = g.stopRoom(g.ctx, s.room)
					g.notify(s.room, "原生会话 ID 未能保存，已停止本聊天运行以免下次启动静默换成新上下文。请检查服务诊断和磁盘状态；维护者确认后可用 /new 明确开启新会话。", nil)
				})
			}
			return
		}
		s.mu.Lock()
		s.session.CCID = ev.CCID
		s.mu.Unlock()
	case "accepted":
		s.mu.Lock()
		d, ok := s.deliveries[ev.UUID]
		accepted := s.accepted[ev.UUID]
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
		_ = g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
			for _, id := range valid {
				if _, e := tx.Exec("UPDATE inbox SET status='consumed' WHERE uuid=? AND room=? AND session=? AND status='sent'", id, s.room.Key, s.session.ID); e != nil {
					return e
				}
			}
			return nil
		})
	case "activity":
		s.mu.Lock()
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
	case "text", "thinking":
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
	err := g.store.Transaction(context.Background(), func(tx *sql.Tx) error { var e error; disposition, e = g.recordUsage(tx, s, ev); return e })
	if err != nil {
		g.audit(s.room.Key, "usage_persistence_failed", core.Args{"session": s.session.ID, "error": err.Error()})
		return
	}
	if disposition == "duplicate" {
		return
	}
	g.audit(s.room.Key, "turn_result", core.Args{"session": s.session.ID, "success": ev.Success, "compact": ev.Compact, "resultId": ev.ResultID, "models": ev.Models, "usage": ev.Usage})
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
	model := ev.Model
	if model == "" {
		model = strings.Join(ev.Models, " / ")
	}
	r.setDetails(replyDetails{Model: model, ProjectPath: s.workspace.Work, Context: ev.Context, Usage: ev.Usage})
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
	drain, err := r.persist(answer, ev.Success, who, title, finished)
	for attempt := 0; err != nil && attempt < len(finalPersistRetry) && !g.isClosing(); attempt++ {
		// Usage is already recorded, so a duplicate result cannot retry this.
		// A transient database failure gets bounded retries here instead.
		time.Sleep(finalPersistRetry[attempt])
		drain, err = r.persist(answer, ev.Success, who, title, finished)
	}
	if err != nil {
		g.audit(s.room.Key, "final_reply_persistence_failed", core.Args{"session": s.session.ID, "error": err.Error()})
		g.abandonResult(s, key, r, finished, nativeIDs, who)
		if excerpt != nil {
			// Already detached from s.renders; nothing else would close it.
			g.task(func() { excerpt.cancel("后台续轮已结束") })
		}
		g.notify(s.room, "回复保存失败，本轮不标记完成；未确认输入不会自动重放。请检查服务诊断和磁盘状态。", nil)
		return
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
		if finish, e := excerpt.persist("本轮后台片段已结束；后续消息的答复见本轮另一张回复卡片。", true, nil, "后台续轮已结束", nil); e == nil {
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
	}
	g.kickWaiting()
}
func (g *Gateway) exitEvent(s *slot, ev core.WorkerEvent) {
	cleanupFailed := ev.CleanupConfirmed != nil && !*ev.CleanupConfirmed
	s.mu.Lock()
	interrupted := len(s.deliveries) > 0 || s.background > 0 || s.pinned
	s.live = false
	s.stopUnconfirmed = cleanupFailed
	s.cancel()
	// A process that never reported its native session never began the
	// conversation, so its inputs were not processed and leave no context.
	neverStarted := !s.nativeReady && s.session.CCID == ""
	renders := []*render{}
	for _, r := range s.renders {
		renders = append(renders, r)
	}
	s.mu.Unlock()
	approvals := g.approvalCount(s.session.ID)
	g.cancelApprovals(s.session.ID)
	g.cancelReceipts(s.session.ID)
	notice := ""
	err := g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		status := "uncertain"
		if neverStarted {
			status = "cancelled"
		}
		res, e := tx.Exec("UPDATE inbox SET status=? WHERE room=? AND session=? AND status IN ('sent','consumed')", status, s.room.Key, s.session.ID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if cleanupFailed || interrupted || approvals > 0 || n > 0 {
			text := "原生工作进程意外退出。当前审批／追问已取消；未确认的输入与外部操作不会自动重放。请先核实结果。会话记录仍保留，可以继续原会话。"
			if neverStarted && n > 0 {
				text = "原生工作进程在开始对话前退出，输入未提交给模型。请检查服务诊断（模型配置、Claude Code 可执行文件）后重新发送。"
			}
			if cleanupFailed {
				text = "原生进程异常退出，但后代清理未确认。本聊天已暂停；不会启动替代进程或重放未确认操作。请维护者核实进程状态。"
			}
			notice, e = g.saveOutput(tx, s.room, s.session.ID, text, false, "", "", "", DisplayCard("工作中断", text), nil, "", "", "recovery", "")
			return e
		}
		return nil
	})
	if err != nil {
		g.audit(s.room.Key, "worker_exit_persistence_failed", core.Args{"error": err.Error()})
	}
	if cleanupFailed {
		g.audit(s.room.Key, "worker_descendants_unconfirmed", core.Args{"session": s.session.ID})
	} else {
		g.removeSlot(s)
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
}

var _ = fmt.Sprint

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
