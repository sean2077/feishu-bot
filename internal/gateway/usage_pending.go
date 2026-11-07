package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func usagePendingKey(ev core.WorkerEvent) string {
	if resultIDPattern.MatchString(ev.ResultID) {
		return ev.ResultID
	}
	// Unidentified observations only set a sticky unknown-coverage bit; they
	// must never be assigned fabricated result IDs and added to the totals.
	return ""
}

// stageResultUsage owns this session's native result until the caller has
// decided and handled its disposition. The independent durable observation
// may already be visible to a sweep, but that sweep cannot win the first
// result insertion and make the actual answer look like a duplicate.
func (g *Gateway) stageResultUsage(s *slot, ev core.WorkerEvent) func() {
	unlock := g.usageLocks.lock(s.session.ID)
	_ = g.store.Transaction(context.Background(), func(tx *sql.Tx) error { return g.stageUsage(tx, s, ev) })
	return unlock
}

// Stage only the reported usage fields, not answer text or tool details. This
// independent commit survives a usage_results write failure. The final reply
// transaction repeats this insert so an answer cannot be marked done without
// either its usage row or a durable, visible pending observation.
func (g *Gateway) stageUsage(tx *sql.Tx, s *slot, ev core.WorkerEvent) error {
	kept := core.WorkerEvent{ResultID: usagePendingKey(ev), Success: ev.Success, Models: reportedModels(ev.Models), Usage: tokens(ev.Usage)}
	if ev.Compact != "" {
		kept.Compact = "compact"
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO usage_pending(session,result_id,room,at,event)
SELECT ?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM usage_results WHERE session=? AND result_id=?)`,
		s.session.ID, kept.ResultID, s.room.Key, time.Now().UnixMilli(), jsonText(kept), s.session.ID, kept.ResultID)
	return err
}

// retryUsage retries bookkeeping only. It never re-enters resultEvent, sends
// native input or recreates a reply, and result IDs retain their existing
// exactly-once accounting semantics.
func (g *Gateway) retryUsage(ctx context.Context) {
	if !g.usageRetryMu.TryLock() {
		return
	}
	defer g.usageRetryMu.Unlock()
	type observation struct {
		session, room, raw string
		row, at            int64
	}
	var pending []observation
	// Advance even past malformed or failing rows, retaining their evidence.
	// Otherwise 64 permanently bad observations could starve every later one.
	// At most one wrap and 64 observations are processed in each invocation.
	for scan := 0; scan < 2; scan++ {
		rows, err := g.store.DB.QueryContext(ctx, "SELECT rowid,session,room,at,event FROM usage_pending WHERE rowid>? ORDER BY rowid LIMIT 64", g.usageCursor)
		if err != nil {
			return
		}
		for rows.Next() {
			var p observation
			if err = rows.Scan(&p.row, &p.session, &p.room, &p.at, &p.raw); err != nil {
				break
			}
			pending = append(pending, p)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return
		}
		if len(pending) != 0 || g.usageCursor == 0 {
			break
		}
		g.usageCursor = 0
	}
	for _, p := range pending {
		if ctx.Err() != nil {
			return
		}
		var ev core.WorkerEvent
		if json.Unmarshal([]byte(p.raw), &ev) != nil {
			// Keep malformed evidence visible as a coverage gap.
			g.usageCursor = p.row
			continue
		}
		s := &slot{session: core.Session{ID: p.session}, room: core.Room{Key: p.room}}
		unlock, err := g.usageLocks.lockContext(ctx, p.session)
		if err != nil {
			return
		}
		_ = g.store.Transaction(ctx, func(tx *sql.Tx) error {
			_, err := g.recordUsageAt(tx, s, ev, p.at)
			return err
		})
		unlock()
		g.usageCursor = p.row
	}
}
