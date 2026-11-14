package gateway

import (
	"database/sql"
	"errors"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
	"time"
)

func (g *Gateway) acceptReceipt(r core.Room, session string, m core.Message) {
	if m.InteractionMessageID != "" {
		return
	}
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
			r, ok := g.roomFromKey(room)
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
			var failure core.DeliveryFailure
			definite := errors.As(e, &failure) && !failure.DeliveryUncertain()
			switch {
			case definite && !failure.DeliveryRetryable():
				// A definite, non-retryable rejection (for example a missing
				// reaction permission) will not succeed on the next sweep.
				// Settle this step so the receipt neither polls forever nor
				// blocks retention; the reaction is cosmetic. An intermediate
				// step (clearing "processing" on the way to "done") only
				// settles itself, so the next step is still attempted.
				g.audit(room, "receipt_rejected", core.Args{"messageId": id, "state": next})
				if next != desired {
					if _, e = g.store.DB.Exec("UPDATE receipts SET applied=? WHERE id=? AND desired=?", next, id, desired); e == nil {
						continue
					}
					return
				}
				if next == "processing" {
					// No processing reaction exists, so record it as cleared:
					// a later "done" then creates Done directly instead of
					// trying to remove a reaction that was never added.
					if g.clearUncreatedProcessing(id) == nil {
						continue
					}
					return
				}
				_, _ = g.store.DB.Exec("UPDATE receipts SET applied=desired WHERE id=? AND desired=?", id, desired)
			case definite && (next == "processing" || next == "done") && !strings.HasPrefix(applied.String, "attempt:"):
				// This create was definitely not written: restore the prior
				// state so the next retry creates again instead of reconciling
				// an unknown write that never happened.
				_, _ = g.store.DB.Exec("UPDATE receipts SET applied=? WHERE id=? AND applied=?", applied, id, "attempt:"+next)
				g.audit(room, "receipt_retry_pending", core.Args{"messageId": id})
			case strings.HasPrefix(applied.String, "attempt:") && g.receiptStale(id):
				// An uncertain create is reconciled, never recreated. After a
				// bounded period stop polling a reaction that never appeared,
				// settling only this step: an absent processing reaction is
				// recorded as cleared, and an intermediate step lets the next
				// step (creating Done) still run.
				g.audit(room, "receipt_unconfirmed_settled", core.Args{"messageId": id, "state": next})
				switch {
				case next != desired:
					if _, e = g.store.DB.Exec("UPDATE receipts SET applied=? WHERE id=? AND desired=?", next, id, desired); e == nil {
						continue
					}
				case next == "processing":
					if g.clearUncreatedProcessing(id) == nil {
						continue
					}
				default:
					_, _ = g.store.DB.Exec("UPDATE receipts SET applied=desired WHERE id=? AND desired=?", id, desired)
				}
			default:
				g.audit(room, "receipt_retry_pending", core.Args{"messageId": id})
			}
			return
		}
		if _, e = g.store.DB.Exec("UPDATE receipts SET applied=? WHERE id=?", next, id); e != nil {
			return
		}
	}
}

// A final reply or cancellation may advance desired while OnIt is in flight.
// Keep that newer goal but remember that this create did not happen, so Done
// does not wait for an imaginary, unconfirmed processing reaction.
func (g *Gateway) clearUncreatedProcessing(id string) error {
	_, err := g.store.DB.Exec(`UPDATE receipts SET applied='clear',
desired=CASE WHEN desired='processing' THEN 'clear' ELSE desired END
WHERE id=? AND applied='attempt:processing'`, id)
	return err
}

// receiptUnconfirmedTTL bounds reconciliation of a reaction whose creation
// result was unknown and that has not been observed since.
var receiptUnconfirmedTTL = time.Hour

func (g *Gateway) receiptStale(id string) bool {
	var at int64
	if g.store.DB.QueryRow("SELECT at FROM receipts WHERE id=?", id).Scan(&at) != nil {
		return false
	}
	return time.Now().UnixMilli()-at > receiptUnconfirmedTTL.Milliseconds()
}
