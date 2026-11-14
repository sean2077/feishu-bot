package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

var (
	errOutputNotFound = errors.New("output is absent or terminal in this chat")
	errOutputBusy     = errors.New("output group has an active or unconfirmed handoff")
	errOutputUnbound  = errors.New("required output origins cannot be verified")
)

// inheritedOutputDiscard is called after the atomic INSERT disposition in
// saveOutput. The origin tombstone also covers a final reply that is created
// after a user discarded an earlier required attachment while native ran on.
func (g *Gateway) inheritedOutputDiscard(db sqlExec, id string) error {
	var room, session, actor, source, raw string
	var at int64
	err := db.QueryRow(`SELECT o.room,o.session,d.actor,d.output,o.origins,o.at FROM outbox o
JOIN output_discarded_origins d ON d.room=o.room AND d.session=o.session
WHERE o.id=? AND o.state='cancelled' AND o.kind IN ('reply','file')
AND d.origin IN (SELECT json_extract(value,'$.id') FROM json_each(o.origins)) LIMIT 1`, id).Scan(&room, &session, &actor, &source, &raw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	detail := jsonText(core.Args{"sourceOutput": source, "scope": "turn_delivery", "nativeStopped": false, "deliveryConfirmed": false})
	if _, err = db.Exec(`INSERT OR IGNORE INTO operator_decisions(kind,id,room,session,actor,at,decision,detail) VALUES('output',?,?,?,?,?,'discard',?)`, id, room, session, actor, at, detail); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE receipts SET desired='clear',at=? WHERE room=? AND session=? AND id IN (SELECT json_extract(value,'$.id') FROM json_each(?))`, at, room, session, raw)
	if err != nil {
		return err
	}
	// Native final replies are inserted in render.persist's transaction. Close
	// an already known preview atomically with that inherited disposition.
	if tx, ok := db.(*sql.Tx); ok {
		r, exists := g.roomFromKey(room)
		if exists {
			_, _, err = g.closeDiscardedOutputTx(context.Background(), tx, id, r, "")
		}
	}
	return err
}

type outputDiscardResult struct {
	IDs     []string
	Origins []core.Origin
	Session string
}

func (g *Gateway) discardOutputCommand(ctx context.Context, r core.Room, m core.Message, arg string) error {
	if !canManage(r, m.SenderID) {
		return g.notice(ctx, r, "放弃输出需要本聊天维护者操作。", &m)
	}
	if len(strings.Fields(arg)) != 1 {
		return g.notice(ctx, r, "用法：/discard-output 输出ID。同轮依赖的回复和文件会一并放弃，仍运行的原生任务继续；不会标记 Done 或撤回已发送消息。", &m)
	}
	result, err := g.discardOutput(ctx, r, arg, m.SenderID)
	if err != nil {
		text := "放弃输出未完成，请查看服务诊断。"
		switch {
		case errors.Is(err, errOutputNotFound):
			text = "未放弃：该输出不属于本聊天、已完成或已取消。请用 /outputs 核实。"
		case errors.Is(err, errOutputBusy):
			text = "暂不能放弃：同轮交付仍在准备或发送，无法稳定确认操作范围。没有取消任何组员；请稍后用 /outputs 核实。"
		case errors.Is(err, errOutputUnbound):
			text = "未放弃：该旧记录的来源或依赖关系无法核实，不能把它当作独立输出删除。请维护者检查服务诊断。"
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			text = "操作超时，未确认放弃成功。请用 /outputs 核实当前状态。"
		default:
			g.audit(r.Key, "output_discard_failed", core.Args{"id": arg, "error": core.Redact(err.Error(), g.secrets)})
		}
		return g.notice(ctx, r, text, &m)
	}
	return g.notice(ctx, r, fmt.Sprintf("已放弃 %d 条关联交付。同轮依赖的回复、必要文件及其后续补交也会保留放弃标记；其他轮次不受影响。仍运行的原生任务继续，本操作不标记 Done、不确认送达，也不撤回此前已发送或可能迟到的消息。", len(result.IDs)), &m)
}

func (g *Gateway) discardOutput(ctx context.Context, r core.Room, id, actor string) (outputDiscardResult, error) {
	var result outputDiscardResult
	var closingStatuses []string
	current, ok := g.roomFromKey(r.Key)
	if !ok || !canManage(current, actor) {
		return result, errOutputNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// deliverOutput reserves outActive before reading a row or attempting I/O.
	// Holding the same mutex excludes a retry until the whole group commits.
	if !g.outMu.TryLock() {
		return result, errOutputBusy
	}
	err := g.store.Transaction(ctx, func(tx *sql.Tx) error {
		var session, state string
		if err := tx.QueryRowContext(ctx, "SELECT session,state FROM outbox WHERE room=? AND id=? AND kind!='mention'", r.Key, id).Scan(&session, &state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errOutputNotFound
			}
			return err
		}
		if state == "done" || state == "cancelled" {
			return errOutputNotFound
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,kind,state,COALESCE(target,''),origins FROM outbox WHERE room=? AND session=? AND state NOT IN ('done','cancelled') AND kind!='mention' ORDER BY at,id LIMIT 201`, r.Key, session)
		if err != nil {
			return err
		}
		type item struct {
			id, kind, state, target string
			origins                 []core.Origin
		}
		var items []item
		for rows.Next() {
			var v item
			var raw string
			if err = rows.Scan(&v.id, &v.kind, &v.state, &v.target, &raw); err != nil {
				rows.Close()
				return err
			}
			if raw != "" && raw != "null" {
				if err = json.Unmarshal([]byte(raw), &v.origins); err != nil {
					rows.Close()
					return errOutputUnbound
				}
			}
			if len(v.origins) > 200 {
				rows.Close()
				return errOutputUnbound
			}
			items = append(items, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(items) > 200 {
			return errOutputUnbound
		}
		selected := map[string]bool{id: true}
		originSet := map[string]bool{}
		targets := map[string]bool{}
		bound := false
		for _, v := range items {
			if v.id == id {
				bound = true
				if v.kind == "reply" || v.kind == "file" {
					if len(v.origins) == 0 {
						return errOutputUnbound
					}
					for _, o := range v.origins {
						if o.ID == "" || o.SenderID == "" {
							return errOutputUnbound
						}
						originSet[o.ID] = true
					}
				}
				if v.target != "" {
					targets[v.target] = true
				}
			}
		}
		if !bound {
			return errOutputNotFound
		}
		if len(originSet) > 0 {
			for _, v := range items {
				if (v.kind == "reply" || v.kind == "file") && len(v.origins) == 0 {
					return errOutputUnbound
				}
			}
		}
		for changed := true; changed; {
			changed = false
			for _, v := range items {
				if selected[v.id] {
					continue
				}
				linked := v.target != "" && targets[v.target]
				if v.kind == "reply" || v.kind == "file" {
					for _, o := range v.origins {
						linked = linked || originSet[o.ID]
					}
				}
				if linked {
					if (v.kind == "reply" || v.kind == "file") && len(v.origins) == 0 {
						return errOutputUnbound
					}
					selected[v.id] = true
					changed = true
					if v.target != "" {
						targets[v.target] = true
					}
					if v.kind == "reply" || v.kind == "file" {
						for _, o := range v.origins {
							if o.ID == "" || o.SenderID == "" {
								return errOutputUnbound
							}
							originSet[o.ID] = true
						}
					}
				}
			}
		}
		for _, v := range items {
			if selected[v.id] && (g.outActive[v.id] || !contains([]string{"pending", "uncertain", "blocked"}, v.state)) {
				return errOutputBusy
			}
		}
		// A delivery can have committed state=done while it is still settling
		// the receipt. Inspect its active reservation too; otherwise a late
		// Done could overwrite this transaction's explicit clear decision.
		for activeID := range g.outActive {
			var activeRoom, activeSession, raw string
			var target sql.NullString
			err := tx.QueryRowContext(ctx, "SELECT room,session,target,origins FROM outbox WHERE id=?", activeID).Scan(&activeRoom, &activeSession, &target, &raw)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if activeRoom != r.Key || activeSession != session {
				continue
			}
			if target.Valid && targets[target.String] {
				return errOutputBusy
			}
			var origins []core.Origin
			if raw != "" && raw != "null" {
				if json.Unmarshal([]byte(raw), &origins) != nil {
					return errOutputUnbound
				}
			}
			for _, origin := range origins {
				if originSet[origin.ID] {
					return errOutputBusy
				}
			}
		}
		now := time.Now().UnixMilli()
		for _, v := range items {
			if !selected[v.id] {
				continue
			}
			res, err := tx.ExecContext(ctx, "UPDATE outbox SET state='cancelled',delivery_error='Delivery explicitly discarded; no receipt was confirmed.',retry_after=0 WHERE id=? AND state=?", v.id, v.state)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errOutputBusy
			}
			detail := jsonText(core.Args{"requestedOutput": id, "scope": "turn_delivery", "nativeStopped": false, "deliveryConfirmed": false, "previousState": v.state})
			if _, err = tx.ExecContext(ctx, `INSERT INTO operator_decisions(kind,id,room,session,actor,at,decision,detail) VALUES('output',?,?,?,?,?,'discard',?)`, v.id, r.Key, session, actor, now, detail); err != nil {
				return err
			}
			if v.kind == "reply" || v.kind == "file" {
				for _, origin := range v.origins {
					if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO output_discarded_origins(room,session,origin,output,actor,at) VALUES(?,?,?,?,?,?)`, r.Key, session, origin.ID, v.id, actor, now); err != nil {
						return err
					}
				}
			}
			for _, origin := range v.origins {
				if _, err = tx.ExecContext(ctx, "UPDATE receipts SET desired='clear',at=? WHERE id=? AND room=? AND session=?", now, origin.ID, r.Key, session); err != nil {
					return err
				}
				result.Origins = append(result.Origins, origin)
			}
			result.IDs = append(result.IDs, v.id)
		}
		for _, output := range result.IDs {
			_, status, err := g.closeDiscardedOutputTx(ctx, tx, output, r, "")
			if err != nil {
				return err
			}
			if status != "" && !contains(closingStatuses, status) {
				closingStatuses = append(closingStatuses, status)
			}
		}
		result.Session = session
		return nil
	})
	g.outMu.Unlock()
	if err != nil {
		return outputDiscardResult{}, err
	}
	for _, origin := range result.Origins {
		g.kickReceipt(origin.ID)
	}
	for _, status := range closingStatuses {
		status := status
		g.task(func() { _, _ = g.deliverOutput(g.ctx, status, r, false) })
	}
	g.audit(r.Key, "output_discarded_by_user", core.Args{"actor": actor, "requested": id, "outputs": result.IDs, "scope": "turn_delivery", "nativeStopped": false, "deliveryConfirmed": false})
	return result, nil
}

// closeDiscardedOutput can also receive the late preview target from render's
// finalization callback. It creates at most one durable status for this decision
// and never creates a new answer card merely to announce an abandonment.
func (g *Gateway) closeDiscardedOutput(ctx context.Context, id string, r core.Room, lateTarget string) (bool, error) {
	var discarded bool
	var status string
	err := g.store.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		discarded, status, err = g.closeDiscardedOutputTx(ctx, tx, id, r, lateTarget)
		return err
	})
	if err == nil && status != "" && !g.isClosing() {
		g.task(func() { _, _ = g.deliverOutput(g.ctx, status, r, false) })
	}
	return discarded, err
}

func (g *Gateway) closeDiscardedOutputTx(ctx context.Context, tx *sql.Tx, id string, r core.Room, lateTarget string) (bool, string, error) {
	var state, session, raw, detail string
	var target, reply, thread sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT o.state,o.session,o.target,o.reply_to,o.thread_id,o.origins,d.detail FROM outbox o JOIN operator_decisions d ON d.kind='output' AND d.id=o.id AND d.decision='discard' WHERE o.id=? AND o.room=?`, id, r.Key).Scan(&state, &session, &target, &reply, &thread, &raw, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if state != "cancelled" {
		return false, "", nil
	}
	var decision core.Args
	if err = json.Unmarshal([]byte(detail), &decision); err != nil {
		return true, "", err
	}
	status := stringArg(decision, "closingStatus")
	if status != "" {
		return true, status, nil
	}
	if !target.Valid || target.String == "" {
		target.String = lateTarget
	}
	if target.String == "" {
		return true, "", nil
	}
	// A reply and an older status can share one target. Persist one closing
	// status, then point every decision at it instead of duplicating writes.
	var existing sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT json_extract(d.detail,'$.closingStatus') FROM operator_decisions d JOIN outbox o ON o.id=d.id WHERE d.kind='output' AND d.decision='discard' AND o.room=? AND o.session=? AND o.target=? AND json_extract(d.detail,'$.closingStatus') IS NOT NULL LIMIT 1`, r.Key, session, target.String).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true, "", err
	}
	status = existing.String
	if status == "" {
		var who []core.Origin
		if raw != "" && raw != "null" {
			if err = json.Unmarshal([]byte(raw), &who); err != nil {
				return true, "", err
			}
		}
		const text = "本轮交付已由维护者明确放弃；没有确认完整送达，也不标记 Done。原生任务按原有生命周期继续，已发出的消息不会被撤回。"
		card := DisplayCard("已放弃交付 · 未确认完成", text)
		card = setReplyCardStatus(card, g.cardTitle(), "已放弃交付 · 未确认完成", "grey")
		card["config"] = core.Args{"streaming_mode": false, "summary": core.Args{"content": text}}
		status, err = g.saveOutput(tx, r, session, text, false, target.String, reply.String, thread.String, card, who, "", "", "status", "pending")
		if err != nil {
			return true, "", err
		}
	}
	decision["closingStatus"] = status
	if _, err = tx.ExecContext(ctx, "UPDATE operator_decisions SET detail=? WHERE kind='output' AND id=?", jsonText(decision), id); err != nil {
		return true, "", err
	}
	_, err = tx.ExecContext(ctx, "UPDATE outbox SET target=COALESCE(target,?) WHERE id=? AND state='cancelled'", target.String, id)
	return true, status, err
}

// acknowledgePreviewTarget persists the confirmed late create receipt even if
// an inherited discard changed preparing to cancelled. The target binding and
// closing status commit together; the ordinary reply stays cancelled.
func (g *Gateway) acknowledgePreviewTarget(ctx context.Context, id string, r core.Room, target string) error {
	// The platform ACK has already been observed. A concurrent /stop may
	// cancel the render context, but must not erase that delivery evidence.
	// render.flush is tracked by Gateway's wait group, so shutdown still waits
	// for this short metadata transaction before closing the store.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var status string
	err := g.store.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE outbox SET target=? WHERE id=? AND room=? AND (target IS NULL OR target=?) AND (state='preparing' OR (state='cancelled' AND EXISTS(SELECT 1 FROM operator_decisions d WHERE d.kind='output' AND d.id=outbox.id AND d.decision='discard')))`, target, id, r.Key, target)
		if err != nil {
			return err
		}
		_, status, err = g.closeDiscardedOutputTx(ctx, tx, id, r, target)
		return err
	})
	if err == nil && status != "" && !g.isClosing() {
		g.task(func() { _, _ = g.deliverOutput(g.ctx, status, r, false) })
	}
	return err
}
