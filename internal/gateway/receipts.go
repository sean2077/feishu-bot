package gateway

import (
	"database/sql"
	"github.com/sean2077/feishu-bot/internal/core"
	"time"
)

func (g *Gateway) acceptReceipt(r core.Room, session string, m core.Message) {
	if m.InteractionMessageID != "" { return }
	_, e := g.store.DB.Exec("INSERT INTO receipts(id,room,session,desired,at,sender) VALUES(?,?,?,'processing',?,?) ON CONFLICT(id) DO UPDATE SET desired='processing',at=excluded.at WHERE receipts.room=excluded.room AND receipts.session=excluded.session AND receipts.desired!='done'", m.ID, r.Key, session, time.Now().UnixMilli(), m.SenderID)
	if e == nil {
		g.kickReceipt(m.ID)
	}
}
func (g *Gateway) settleReceipts(room, session string, who []core.Origin, success bool) {
	state := "clear"
	if success {
		state = "done"
	}
	for _, o := range who {
		if _, e := g.store.DB.Exec("UPDATE receipts SET desired=?,at=? WHERE id=? AND room=? AND session=?", state, time.Now().UnixMilli(), o.ID, room, session); e == nil {
			g.kickReceipt(o.ID)
		}
	}
}
func (g *Gateway) cancelReceipts(session string) {
	_, _ = g.store.DB.Exec("UPDATE receipts SET desired='clear',at=? WHERE session=? AND desired='processing'", time.Now().UnixMilli(), session)
	rows, e := g.store.DB.Query("SELECT id FROM receipts WHERE session=? AND desired='clear'", session)
	if e != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		g.kickReceipt(id)
	}
}
func (g *Gateway) retryReceipts() {
	g.receiptMu.Lock()
	after := g.receiptCursor
	g.receiptMu.Unlock()
	read := func(after outputCursor) []outputCursor {
		q := "SELECT id,at FROM receipts WHERE (applied IS NULL OR desired!=applied)"
		args := []any{}
		if after.ID != "" {
			q += " AND (at>? OR (at=? AND id>?))"
			args = append(args, after.At, after.At, after.ID)
		}
		q += " ORDER BY at,id LIMIT 50"
		rows, e := g.store.DB.Query(q, args...)
		if e != nil {
			return nil
		}
		defer rows.Close()
		var out []outputCursor
		for rows.Next() {
			var o outputCursor
			if rows.Scan(&o.ID, &o.At) == nil {
				out = append(out, o)
			}
		}
		return out
	}
	rows := read(after)
	if len(rows) == 0 && after.ID != "" {
		rows = read(outputCursor{})
	}
	g.receiptMu.Lock()
	g.receiptCursor = outputCursor{}
	if len(rows) > 0 {
		g.receiptCursor = rows[len(rows)-1]
	}
	g.receiptMu.Unlock()
	for _, r := range rows {
		g.kickReceipt(r.ID)
	}
}
func (g *Gateway) kickReceipt(id string) {
	if g.isClosing() {
		return
	}
	g.receiptMu.Lock()
	w := g.receiptActive[id]
	if w != nil {
		w.dirty = true
		g.receiptMu.Unlock()
		return
	}
	w = &dispatchWake{running: true}
	g.receiptActive[id] = w
	g.receiptMu.Unlock()
	if !g.task(func() {
		for {
			g.flushReceipt(id)
			g.receiptMu.Lock()
			if !w.dirty || g.ctx.Err() != nil {
				delete(g.receiptActive, id)
				g.receiptMu.Unlock()
				return
			}
			w.dirty = false
			g.receiptMu.Unlock()
		}
	}) {
		g.receiptMu.Lock()
		delete(g.receiptActive, id)
		g.receiptMu.Unlock()
	}
}
func (g *Gateway) flushReceipt(id string) {
	unlock := g.receiptLocks.lock(id)
	defer unlock()
	for g.ctx.Err() == nil {
		var desired, room string
		var applied, sender sql.NullString
		e := g.store.DB.QueryRow("SELECT desired,applied,room,sender FROM receipts WHERE id=?", id).Scan(&desired, &applied, &room, &sender)
		if e != nil || desired == applied.String {
			return
		}
		if desired != "clear" {
			r, ok := RoomFromKey(g.config, room)
			if !ok || !AuthorizedAuthor(r, sender.String) {
				_, _ = g.store.DB.Exec("UPDATE receipts SET desired='clear' WHERE id=?", id)
				continue
			}
		}
		next := desired
		if desired == "done" && (applied.String == "processing" || applied.String == "attempt:processing") {
			next = "clear"
		}
		if next == "processing" || next == "done" {
			attempt := "attempt:" + next
			if applied.String != attempt {
				if _, e = g.store.DB.Exec("UPDATE receipts SET applied=? WHERE id=?", attempt, id); e != nil {
					return
				}
			}
		}
		e = providerVoid(g.ctx, func() error { return g.transport.Receipt(g.ctx, id, next, applied.String) })
		if g.ctx.Err() != nil {
			return
		}
		if e != nil {
			g.audit(room, "receipt_retry_pending", core.Args{"messageId": id})
			return
		}
		if _, e = g.store.DB.Exec("UPDATE receipts SET applied=? WHERE id=?", next, id); e != nil {
			return
		}
	}
}
