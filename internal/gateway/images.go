package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
	"strings"
)

func imageEligible(m core.Message) bool {
	if !m.ImageOnly || m.InputIssue != "" || len(m.Resources) == 0 || strings.HasPrefix(m.Text, "/") {
		return false
	}
	for _, r := range m.Resources {
		if r.Kind != "image" {
			return false
		}
	}
	return true
}
func imageCompatible(a, b core.Delivery) bool {
	return a.RoomKey == b.RoomKey && a.SessionID == b.SessionID && a.Message.SenderID == b.Message.SenderID && a.Message.ReplyTo == b.Message.ReplyTo && a.Message.ThreadID == b.Message.ThreadID
}
func imageMembers(pending []core.Delivery, first core.Delivery) ([]core.Delivery, error) {
	if first.ImageBatch == nil {
		return []core.Delivery{first}, nil
	}
	out := []core.Delivery{}
	found := false
	for _, d := range pending {
		if d.UUID == first.UUID {
			found = true
		}
		if !found {
			continue
		}
		if d.ImageBatch == nil || d.ImageBatch.ID != first.ImageBatch.ID {
			break
		}
		if !imageCompatible(d, first) || d.Command != nil || d.SupplementTo != "" || !imageEligible(d.Message) {
			return nil, errors.New("invalid persisted image batch")
		}
		out = append(out, d)
	}
	return out, nil
}
func queuedTx(ctx context.Context, tx *sql.Tx, room string) ([]core.Delivery, error) {
	rows, e := tx.QueryContext(ctx, "SELECT payload FROM inbox WHERE room=? AND status='queued' AND payload IS NOT NULL ORDER BY rowid", room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var ds []core.Delivery
	for rows.Next() {
		var raw string
		var d core.Delivery
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &d); e != nil {
			return nil, e
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}
func saveQueuedTx(ctx context.Context, tx *sql.Tx, d core.Delivery) error {
	_, e := tx.ExecContext(ctx, "UPDATE inbox SET status='queued',uuid=?,session=?,payload=? WHERE room=? AND id=?", d.UUID, d.SessionID, jsonText(d), d.RoomKey, d.Message.ID)
	return e
}
func sealImagesTx(ctx context.Context, tx *sql.Tx, ds []core.Delivery) error {
	for _, d := range ds {
		if d.ImageBatch != nil && !d.ImageBatch.Closed {
			b := *d.ImageBatch
			b.Closed = true
			d.ImageBatch = &b
			if e := saveQueuedTx(ctx, tx, d); e != nil {
				return e
			}
		}
	}
	return nil
}
func (g *Gateway) sealImages(room string) error {
	return g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		ds, e := queuedTx(context.Background(), tx, room)
		if e != nil {
			return e
		}
		return sealImagesTx(context.Background(), tx, ds)
	})
}

var errInboxFull = errors.New("chat pending input limit reached")

func (g *Gateway) queueInput(ctx context.Context, d core.Delivery, now int64) error {
	r, ok := RoomFromKey(g.config, d.RoomKey)
	if !ok {
		return errors.New("input room is no longer authorized")
	}
	return g.store.Transaction(ctx, func(tx *sql.Tx) error {
		// The count and insert share a transaction, not just a per-topic lock:
		// message and card lanes may admit different topics concurrently.
		pendingCount, e := store.CountPendingChatTx(ctx, tx, r.WorkspaceKey())
		if e != nil {
			return e
		}
		var alreadyPending int
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM inbox WHERE room=? AND id=? AND status IN ('queued','sent','consumed'))", d.RoomKey, d.Message.ID).Scan(&alreadyPending); e != nil {
			return e
		}
		if pendingCount-alreadyPending >= g.config.Limits.InboxPerRoom {
			return errInboxFull
		}
		pending, e := queuedTx(ctx, tx, d.RoomKey)
		if e != nil {
			return e
		}
		l := g.config.Limits
		if l.ImageBatchMS == 0 || d.Command != nil || d.SupplementTo != "" || !imageEligible(d.Message) {
			if e = sealImagesTx(ctx, tx, pending); e != nil {
				return e
			}
		} else {
			merged := false
			if len(pending) > 0 {
				tail := pending[len(pending)-1]
				old := tail.ImageBatch
				if old != nil && !old.Closed && old.Deadline > now && imageCompatible(tail, d) {
					var members []core.Delivery
					images := len(d.Message.Resources)
					compatible := true
					for _, m := range pending {
						if m.ImageBatch != nil && m.ImageBatch.ID == old.ID {
							members = append(members, m)
							images += len(m.Message.Resources)
							if !imageCompatible(m, d) || m.Command != nil || m.SupplementTo != "" {
								compatible = false
							}
						}
					}
					if compatible && images <= min(old.MaxImages, l.ImageBatchMaxImages) {
						b := *old
						b.MaxBytes = min(old.MaxBytes, l.ImageBatchBytes)
						b.Closed = images >= min(old.MaxImages, l.ImageBatchMaxImages)
						d.ImageBatch = &b
						for _, m := range members {
							copy := b
							m.ImageBatch = &copy
							if e = saveQueuedTx(ctx, tx, m); e != nil {
								return e
							}
						}
						merged = true
					}
				}
			}
			if !merged {
				if e = sealImagesTx(ctx, tx, pending); e != nil {
					return e
				}
				d.ImageBatch = &core.ImageBatch{ID: core.NewID(), Deadline: now + l.ImageBatchMS, Closed: len(d.Message.Resources) >= l.ImageBatchMaxImages, MaxImages: l.ImageBatchMaxImages, MaxBytes: l.ImageBatchBytes}
			}
		}
		inserted, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO inbox(room,id,message,status,at) VALUES(?,?,?,'queued',?)", d.RoomKey, d.Message.ID, jsonText(d.Message), d.Message.Time)
		if e != nil {
			return e
		}
		n, e := inserted.RowsAffected()
		if e != nil {
			return e
		}
		if n > 0 && !d.Message.SenderIsBot && d.Command == nil {
			if e := store.RecordSessionActivity(tx, d.RoomKey, d.SessionID, now); e != nil {
				return e
			}
		}
		if e = saveQueuedTx(ctx, tx, d); e != nil {
			return e
		}
		for _, m := range d.Background {
			if _, e = tx.ExecContext(ctx, "UPDATE inbox SET status='context' WHERE room=? AND id=? AND status='background'", d.RoomKey, m.ID); e != nil {
				return e
			}
		}
		return ctx.Err()
	})
}
