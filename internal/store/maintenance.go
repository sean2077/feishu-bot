package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	maintenanceCandidates = 128
	maintenanceChanges    = 64
	maintenanceTimeout    = 500 * time.Millisecond
	dayMS                 = int64(24 * time.Hour / time.Millisecond)
)

// MaintenanceReport counts only committed work. A failed policy rolls back both
// its changes and its cursor, while earlier policies remain committed.
type MaintenanceReport struct {
	DurationMS int64                     `json:"durationMs"`
	Candidates int64                     `json:"candidates"`
	Changed    int64                     `json:"changed"`
	Deadline   bool                      `json:"deadline,omitempty"`
	Progress   bool                      `json:"progress,omitempty"`
	Policies   []MaintenancePolicyReport `json:"policies"`
}

type MaintenancePolicyReport struct {
	Name       string `json:"name"`
	Candidates int64  `json:"candidates"`
	Changed    int64  `json:"changed"`
	Deadline   bool   `json:"deadline,omitempty"`
	Paused     bool   `json:"paused,omitempty"`
	Error      string `json:"error,omitempty"`
}

func initializeMaintenance(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS maintenance_cursors(name TEXT PRIMARY KEY,at INTEGER NOT NULL,row_id INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS operator_decisions(kind TEXT NOT NULL,id TEXT NOT NULL,room TEXT NOT NULL,session TEXT NOT NULL,actor TEXT NOT NULL,at INTEGER NOT NULL,decision TEXT NOT NULL,detail TEXT NOT NULL,PRIMARY KEY(kind,id));
CREATE TABLE IF NOT EXISTS output_discarded_origins(room TEXT NOT NULL,session TEXT NOT NULL,origin TEXT NOT NULL,output TEXT NOT NULL,actor TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(room,session,origin,output));
CREATE INDEX IF NOT EXISTS output_discarded_origin_lookup ON output_discarded_origins(room,origin);
CREATE INDEX IF NOT EXISTS output_discarded_origins_room_at ON output_discarded_origins(room,at);
CREATE INDEX IF NOT EXISTS operator_decisions_room_at ON operator_decisions(room,at);
CREATE INDEX IF NOT EXISTS context_refs_age ON context_refs(at);
CREATE INDEX IF NOT EXISTS receipts_terminal_age ON receipts(at) WHERE desired=applied AND desired!='processing';
CREATE INDEX IF NOT EXISTS inbox_background_age ON inbox(at) WHERE status IN ('background','context');
CREATE INDEX IF NOT EXISTS inbox_terminal_age ON inbox(at) WHERE status IN ('done','cancelled');
CREATE INDEX IF NOT EXISTS inbox_terminal_body_age ON inbox(at) WHERE status IN ('done','cancelled') AND (message!='{}' OR payload IS NOT NULL);
CREATE INDEX IF NOT EXISTS inbox_uuid ON inbox(uuid);
CREATE INDEX IF NOT EXISTS inbox_queued_room ON inbox(room) WHERE status='queued';
CREATE INDEX IF NOT EXISTS inbox_active_room ON inbox(room,status) WHERE status IN ('queued','sent','consumed');
CREATE INDEX IF NOT EXISTS inbox_room_pending_age ON inbox(room,at) WHERE status IN ('queued','sent','consumed','uncertain');
CREATE INDEX IF NOT EXISTS inbox_session_active ON inbox(session) WHERE status IN ('queued','sent','consumed','uncertain');
CREATE INDEX IF NOT EXISTS outputs_age ON outputs(at);
CREATE INDEX IF NOT EXISTS audit_age ON audit(at);
CREATE INDEX IF NOT EXISTS audit_room_age ON audit(room,at);
CREATE INDEX IF NOT EXISTS requests_terminal_expiry ON requests(expires) WHERE state IN ('approved','denied','expired','cancelled');
CREATE INDEX IF NOT EXISTS requests_session_pending ON requests(session) WHERE state='pending';
CREATE INDEX IF NOT EXISTS requests_room_pending ON requests(room) WHERE state='pending';
CREATE INDEX IF NOT EXISTS requests_room_expiry ON requests(room,expires);
CREATE INDEX IF NOT EXISTS action_runs_terminal_age ON action_runs(at) WHERE state IN ('done','failed');
CREATE INDEX IF NOT EXISTS action_runs_session_active ON action_runs(session) WHERE state IN ('running','uncertain');
CREATE INDEX IF NOT EXISTS action_runs_room_active ON action_runs(room) WHERE state IN ('running','uncertain');
CREATE INDEX IF NOT EXISTS action_runs_room_age ON action_runs(room,at);
CREATE INDEX IF NOT EXISTS feedback_at ON feedback(at);
CREATE INDEX IF NOT EXISTS feedback_project_value_at ON feedback(project,value,at);
CREATE TABLE IF NOT EXISTS feedback_exports(project TEXT PRIMARY KEY,at INTEGER NOT NULL,id TEXT NOT NULL,file TEXT NOT NULL,rows INTEGER NOT NULL,updated INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS outbox_terminal_age ON outbox(at) WHERE state IN ('done','cancelled');
CREATE INDEX IF NOT EXISTS outbox_session_active ON outbox(session) WHERE state NOT IN ('done','cancelled');
CREATE INDEX IF NOT EXISTS outbox_room_active ON outbox(room) WHERE state NOT IN ('done','cancelled');
CREATE INDEX IF NOT EXISTS outbox_reply_active ON outbox(room,reply_to) WHERE state NOT IN ('done','cancelled');
CREATE INDEX IF NOT EXISTS outbox_room_state_age ON outbox(room,state,at,id);
CREATE INDEX IF NOT EXISTS outbox_room_state_retry ON outbox(room,state,retry_after,at,id);
CREATE INDEX IF NOT EXISTS outbox_session_kind ON outbox(room,session,kind,state);
CREATE INDEX IF NOT EXISTS feishu_cards_room ON feishu_cards(room);
CREATE INDEX IF NOT EXISTS receipts_room_age ON receipts(room,at);
CREATE INDEX IF NOT EXISTS outputs_room_age ON outputs(room,at);
CREATE INDEX IF NOT EXISTS usage_room_age ON usage_results(room,at);
`)
	return err
}

type maintenancePolicy struct {
	name, table, at, eligible, protect, change string
	cutoff                                     int64
}

// MaintainOptions carries per-sweep policy adjustments. The zero value keeps
// the existing behavior.
type MaintainOptions struct {
	// PausedPolicies are left untouched this sweep. A required archive uses
	// this as a second line of defense while its failure is being fixed.
	PausedPolicies []string
	// FeedbackExport protects feedback rows whose (at,id) is above the saved
	// per-project export watermark, so retention only deletes rows a prior
	// archive covered.
	FeedbackExport bool
}

// feedbackUnexported protects rows that no successful per-project export
// covered. The watermark is the (at,id) of the last row written to an archive;
// rows at or below it are eligible for normal retention.
const feedbackUnexported = `NOT EXISTS(SELECT 1 FROM feedback_exports e WHERE e.project=feedback.project AND (feedback.at<e.at OR (feedback.at=e.at AND feedback.id<=e.id)))`

// A selected native conversation may stay open for months. Only actual pending
// work protects its terminal rows; merely being selected does not. An uncertain
// input or action has no later resolution, so it keeps only its own row as
// evidence instead of freezing every terminal row in its session or room. The
// redundant IN term keeps each query matched to its partial index.
func pendingSession(table string) string {
	return `EXISTS(SELECT 1 FROM inbox i WHERE i.session=` + table + `.session AND i.status IN ('queued','sent','consumed','uncertain') AND i.status!='uncertain') OR
EXISTS(SELECT 1 FROM outbox o WHERE o.session=` + table + `.session AND o.state NOT IN ('done','cancelled')) OR
EXISTS(SELECT 1 FROM requests r WHERE r.session=` + table + `.session AND r.state='pending') OR
EXISTS(SELECT 1 FROM action_runs a WHERE a.session=` + table + `.session AND a.state IN ('running','uncertain') AND a.state='running')`
}

// Background/history metadata predates a selected session and often has no
// session key. Keep it while any durable work in the room can still refer to it.
func pendingRoom(table string) string {
	return `EXISTS(SELECT 1 FROM inbox i WHERE i.room=` + table + `.room AND i.status IN ('queued','sent','consumed','uncertain') AND i.status!='uncertain') OR
EXISTS(SELECT 1 FROM outbox o INDEXED BY outbox_room_active WHERE o.room=` + table + `.room AND o.state NOT IN ('done','cancelled')) OR
EXISTS(SELECT 1 FROM requests r WHERE r.room=` + table + `.room AND r.state='pending') OR
EXISTS(SELECT 1 FROM action_runs a WHERE a.room=` + table + `.room AND a.state IN ('running','uncertain') AND a.state='running')`
}

func maintenancePolicies(now, backgroundMS int64, feedbackExport ...bool) []maintenancePolicy {
	inputRefs := pendingSession("inbox") + ` OR EXISTS(SELECT 1 FROM outbox o WHERE o.room=inbox.room AND o.reply_to=inbox.id AND o.state NOT IN ('done','cancelled'))`
	feedbackProtect := ""
	if len(feedbackExport) > 0 && feedbackExport[0] {
		feedbackProtect = feedbackUnexported
	}
	return []maintenancePolicy{
		{"card_controls", "card_controls", "expires", "1", "", "", now},
		{"context_refs", "context_refs", "at", "1", pendingRoom("context_refs"), "", now - dayMS},
		{"receipts", "receipts", "at", "desired=applied AND desired!='processing'", pendingSession("receipts"), "", now - 7*dayMS},
		{"inbox_background", "inbox", "at", "status IN ('background','context')", inputRefs + " OR " + pendingRoom("inbox"), "", now - max(int64(0), backgroundMS)},
		{"inbox_bodies", "inbox", "at", "status IN ('done','cancelled') AND (message!='{}' OR payload IS NOT NULL)", inputRefs, "message='{}',payload=NULL", now - dayMS},
		{"inbox_terminal", "inbox", "at", "status IN ('done','cancelled')", inputRefs + ` OR EXISTS(SELECT 1 FROM operator_decisions d WHERE d.kind='input' AND d.id=inbox.uuid) OR EXISTS(SELECT 1 FROM output_discarded_origins d WHERE d.room=inbox.room AND d.origin=inbox.id)`, "", now - 30*dayMS},
		{"outputs", "outputs", "at", "1", `EXISTS(SELECT 1 FROM outbox o WHERE o.target=outputs.id AND o.state NOT IN ('done','cancelled')) OR EXISTS(SELECT 1 FROM outbox o WHERE o.file_target=outputs.id AND o.state NOT IN ('done','cancelled'))`, "", now - 30*dayMS},
		{"outbox", "outbox", "at", "state IN ('done','cancelled')", `file_path IS NOT NULL OR file_state IN ('sending','uncertain') OR EXISTS(SELECT 1 FROM operator_decisions d WHERE d.kind='output' AND d.id=outbox.id)`, "", now - 7*dayMS},
		{"discarded_output_bodies", "outbox", "at", "state IN ('done','cancelled') AND (text!='' OR card IS NOT NULL OR initial_card IS NOT NULL)", `state!='cancelled' OR file_path IS NOT NULL OR NOT EXISTS(SELECT 1 FROM operator_decisions d WHERE d.kind='output' AND d.id=outbox.id AND d.decision='discard')`, "text='',card=NULL,initial_card=NULL", now - 7*dayMS},
		{"audit", "audit", "at", "1", "", "", now - 90*dayMS},
		{"feedback", "feedback", "at", "1", feedbackProtect, "", now - 90*dayMS},
		{"requests", "requests", "expires", "state IN ('approved','denied','expired','cancelled')", pendingSession("requests"), "", now - 30*dayMS},
		{"action_runs", "action_runs", "at", "state IN ('done','failed')", pendingSession("action_runs"), "", now - 90*dayMS},
	}
}

// Maintain bounds both old candidates and mutations for every policy. Cursors
// survive restart and advance even across protected rows, so a long-lived
// uncertain record cannot starve later eligible data. It never prunes usage
// results: those rows are also native-result deduplication keys. Paused
// policies are left untouched this sweep; FeedbackExport additionally protects
// rows above the archive watermark.
func (s *Store) Maintain(ctx context.Context, now, backgroundMS int64, options ...MaintainOptions) (report MaintenanceReport, err error) {
	started := time.Now()
	defer func() { report.DurationMS = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, maintenanceTimeout)
	defer cancel()
	var option MaintainOptions
	if len(options) > 0 {
		option = options[0]
	}
	policies := maintenancePolicies(now, backgroundMS, option.FeedbackExport)
	paused := map[string]bool{}
	for _, name := range option.PausedPolicies {
		paused[name] = true
	}
	var next int
	readErr := s.DB.QueryRowContext(ctx, "SELECT at FROM maintenance_cursors WHERE name='next_policy'").Scan(&next)
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		if errors.Is(readErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			report.Deadline = true
			return report, nil
		}
		return report, readErr
	}
	if next < 0 || next >= len(policies) {
		next = 0
	}
	committed := 0
	for i := 0; i < len(policies); i++ {
		index := (next + i) % len(policies)
		p := policies[index]
		if paused[p.name] {
			report.Policies = append(report.Policies, MaintenancePolicyReport{Name: p.name, Paused: true})
			continue
		}
		// A pass with a paused policy is intentionally incomplete: do not
		// advertise a full maintenance circle while retention is behind.
		entry, policyErr := s.maintainPolicy(ctx, p, (index+1)%len(policies), len(paused) == 0)
		if policyErr != nil {
			entry.Error = policyErr.Error()
			if errors.Is(policyErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				// The budget is a soft bound for one sweep, not a failure of the
				// policy. Cursors already committed by earlier policies point at
				// the interrupted one, so the next sweep resumes there.
				entry.Deadline = true
				report.Deadline = true
				report.Progress = committed > 0
				report.Policies = append(report.Policies, entry)
				report.Candidates += entry.Candidates
				report.Changed += entry.Changed
				return report, nil
			}
		}
		report.Policies = append(report.Policies, entry)
		report.Candidates += entry.Candidates
		report.Changed += entry.Changed
		if policyErr != nil {
			return report, fmt.Errorf("maintenance %s: %w", p.name, policyErr)
		}
		committed++
	}
	report.Progress = true
	return report, nil
}

// LastFullMaintenance returns the completion time of the last ordered pass
// through every maintenance policy. A pass may span several bounded sweeps;
// the marker is written when the persisted cursor wraps back to the first
// policy. The second result is false before the first complete pass
// (including after upgrading an older database).
func (s *Store) LastFullMaintenance(ctx context.Context) (int64, bool, error) {
	var at int64
	err := s.DB.QueryRowContext(ctx, "SELECT at FROM maintenance_cursors WHERE name='last_full'").Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return at, err == nil, err
}

func (s *Store) maintainPolicy(ctx context.Context, p maintenancePolicy, next int, markFull bool) (MaintenancePolicyReport, error) {
	entry := MaintenancePolicyReport{Name: p.name}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		at, rowID := int64(-9223372036854775808), int64(0)
		err := tx.QueryRowContext(ctx, "SELECT at,row_id FROM maintenance_cursors WHERE name=?", p.name).Scan(&at, &rowID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// All identifiers/expressions come from maintenancePolicies above.
		q := "SELECT rowid," + p.at + " FROM " + p.table + " WHERE (" + p.eligible + ") AND " + p.at + "<? AND (" + p.at + ",rowid)>(?,?) ORDER BY " + p.at + ",rowid LIMIT ?"
		rows, err := tx.QueryContext(ctx, q, p.cutoff, at, rowID, maintenanceCandidates)
		if err != nil {
			return err
		}
		type candidate struct{ rowID, at int64 }
		var candidates []candidate
		for rows.Next() {
			var v candidate
			if err = rows.Scan(&v.rowID, &v.at); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			if _, err = tx.ExecContext(ctx, "DELETE FROM maintenance_cursors WHERE name=?", p.name); err != nil {
				return err
			}
		} else {
			for _, v := range candidates {
				if err = ctx.Err(); err != nil {
					return err
				}
				entry.Candidates++
				at, rowID = v.at, v.rowID
				q = "DELETE FROM " + p.table
				if p.change != "" {
					q = "UPDATE " + p.table + " SET " + p.change
				}
				q += " WHERE rowid=? AND (" + p.eligible + ") AND " + p.at + "<?"
				if p.protect != "" {
					q += " AND NOT (" + p.protect + ")"
				}
				res, err := tx.ExecContext(ctx, q, v.rowID, p.cutoff)
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return err
				}
				entry.Changed += n
				if entry.Changed >= maintenanceChanges {
					break
				}
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO maintenance_cursors(name,at,row_id) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET at=excluded.at,row_id=excluded.row_id`, p.name, at, rowID); err != nil {
				return err
			}
		}
		// next wraps to the first policy only after the cursor has committed
		// every policy in order. A failed policy rolls back this transaction,
		// including the marker, so a non-deadline error cannot fake a full pass.
		if next == 0 && markFull {
			if _, err = tx.ExecContext(ctx, `INSERT INTO maintenance_cursors(name,at,row_id) VALUES('last_full',?,0) ON CONFLICT(name) DO UPDATE SET at=excluded.at,row_id=0`, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO maintenance_cursors(name,at,row_id) VALUES('next_policy',?,0) ON CONFLICT(name) DO UPDATE SET at=excluded.at,row_id=0`, next)
		return err
	})
	if err != nil {
		entry.Candidates = 0
		entry.Changed = 0
	}
	return entry, err
}

// TableStat deliberately describes logical rows, not reclaimed file bytes/RSS.
// OldestAt is nil for empty tables and for legacy card snapshots without a clock.
type TableStat struct {
	Table     string `json:"table"`
	Count     int64  `json:"count"`
	OldestAt  *int64 `json:"oldestAt"`
	Clock     string `json:"clock"`
	Retention string `json:"retention"`
}

// TableStats is an on-demand, room-scoped diagnostic. Counts may need a scan,
// so a deadline bounds their cost; a timeout returns the already read tables
// together with the error instead of inventing zero counts.
func (s *Store) TableStats(ctx context.Context, room string) ([]TableStat, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	defs := []struct{ table, clock, retention string }{
		{"sessions", "created", "retained"},
		{"inbox", "at", "background window; terminal body 1d / marker 30d; active, uncertain and operator cancellation/discard markers retained"},
		{"context_refs", "at", "1d; active or uncertain room evidence retains referenced metadata"},
		{"receipts", "at", "settled 7d; active or uncertain session references retained"},
		{"outputs", "at", "30d; unfinished outbox references retained"},
		{"outbox", "at", "terminal 7d after export cleanup; unresolved files and operator decisions retained"},
		{"audit", "at", "90d"},
		{"requests", "expires", "terminal 30d after expiry; active or uncertain session references retained"},
		{"action_runs", "at", "terminal 90d; running and uncertain retained"},
		{"usage_results", "at", "retained for result deduplication and unknown usage coverage"},
		{"usage_coverage", "started", "retained"},
		{"card_controls", "expires", "until expiry"},
		{"feishu_cards", "", "transport terminal retention; legacy clock unavailable"},
		{"operator_decisions", "at", "retained for explicit cancellation/discard evidence"},
		{"output_discarded_origins", "at", "retained to prevent completion/replay after required output abandonment"},
	}
	stats := make([]TableStat, 0, len(defs))
	for _, d := range defs {
		v := TableStat{Table: d.table, Clock: d.clock, Retention: d.retention}
		clock := "NULL"
		if d.clock != "" {
			clock = "min(" + d.clock + ")"
		}
		q := "SELECT count(*)," + clock + " FROM " + d.table
		args := []any{}
		if room != "" {
			q += " WHERE room=?"
			args = append(args, room)
		}
		var oldest sql.NullInt64
		if err := s.DB.QueryRowContext(ctx, q, args...).Scan(&v.Count, &oldest); err != nil {
			return stats, fmt.Errorf("table diagnostics %s: %w", d.table, err)
		}
		if oldest.Valid {
			at := oldest.Int64
			v.OldestAt = &at
		}
		stats = append(stats, v)
	}
	return stats, nil
}
