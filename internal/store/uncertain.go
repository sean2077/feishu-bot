package store

import (
	"context"
	"database/sql"
)

const (
	uncertainRoomLimit   = 20
	uncertainSampleLimit = 5
)

// UncertainBucket is a bounded, content-free view of stuck records. Only IDs,
// timestamps and room keys are read; message bodies and output text never enter
// this diagnostic.
type UncertainBucket struct {
	Count     int64    `json:"count"`
	OldestAt  *int64   `json:"oldestAt"`
	RoomCount int      `json:"roomCount"`
	Rooms     []string `json:"rooms"`
	Samples   []string `json:"samples"`
}

type UncertainSummary struct {
	Inbox  UncertainBucket `json:"inbox"`
	Outbox UncertainBucket `json:"outbox"`
}

type UncertainAckResult struct {
	Inbox  int64 `json:"inbox"`
	Outbox int64 `json:"outbox"`
}

// UncertainSummary counts inbox rows with status=uncertain and outbox rows with
// state=uncertain|blocked. A non-empty room restricts the result to that room;
// a non-nil before restricts it to at<before. Rooms are deduplicated and
// bounded; samples are short, redacted ID prefixes.
func (s *Store) UncertainSummary(ctx context.Context, room string, before *int64) (UncertainSummary, error) {
	inbox, err := s.uncertainBucket(ctx, "inbox", "status='uncertain'", room, before)
	if err != nil {
		return UncertainSummary{}, err
	}
	outbox, err := s.uncertainBucket(ctx, "outbox", "state IN ('uncertain','blocked')", room, before)
	if err != nil {
		return UncertainSummary{}, err
	}
	return UncertainSummary{Inbox: inbox, Outbox: outbox}, nil
}

func (s *Store) uncertainBucket(ctx context.Context, table, predicate, room string, before *int64) (UncertainBucket, error) {
	bucket := UncertainBucket{Rooms: []string{}, Samples: []string{}}
	where := " WHERE " + predicate
	args := []any{}
	if room != "" {
		where += " AND room=?"
		args = append(args, room)
	}
	if before != nil {
		where += " AND at<?"
		args = append(args, *before)
	}
	var oldest sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*),min(at),count(DISTINCT room) FROM "+table+where, args...).Scan(&bucket.Count, &oldest, &bucket.RoomCount); err != nil {
		return UncertainBucket{}, err
	}
	if oldest.Valid {
		at := oldest.Int64
		bucket.OldestAt = &at
	}
	roomArgs := append(append([]any{}, args...), uncertainRoomLimit)
	rows, err := s.DB.QueryContext(ctx, "SELECT DISTINCT room FROM "+table+where+" ORDER BY room LIMIT ?", roomArgs...)
	if err != nil {
		return UncertainBucket{}, err
	}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return UncertainBucket{}, err
		}
		bucket.Rooms = append(bucket.Rooms, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return UncertainBucket{}, err
	}
	sampleArgs := append(append([]any{}, args...), uncertainSampleLimit)
	rows, err = s.DB.QueryContext(ctx, "SELECT id FROM "+table+where+" ORDER BY at,id LIMIT ?", sampleArgs...)
	if err != nil {
		return UncertainBucket{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return UncertainBucket{}, err
		}
		bucket.Samples = append(bucket.Samples, uncertainSample(id))
	}
	return bucket, rows.Err()
}

// uncertainSample keeps enough of an ID for an operator to correlate a record
// without exposing a full message or output identifier in shared diagnostics.
func uncertainSample(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// AckUncertain marks records older than before as acknowledged in one
// transaction. It never touches message bodies or output text and never
// transitions a record that is not currently uncertain (inbox) or uncertain or
// blocked (outbox).
func (s *Store) AckUncertain(ctx context.Context, before int64) (UncertainAckResult, error) {
	var result UncertainAckResult
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		inbox, err := tx.ExecContext(ctx, "UPDATE inbox SET status='acknowledged' WHERE status='uncertain' AND at<?", before)
		if err != nil {
			return err
		}
		result.Inbox, err = inbox.RowsAffected()
		if err != nil {
			return err
		}
		outbox, err := tx.ExecContext(ctx, "UPDATE outbox SET state='acknowledged' WHERE state IN ('uncertain','blocked') AND at<?", before)
		if err != nil {
			return err
		}
		result.Outbox, err = outbox.RowsAffected()
		return err
	})
	return result, err
}
