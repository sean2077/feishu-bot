package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func initializeRuntimeState(tx *sql.Tx) error {
	var existed int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='native_identity_pending'").Scan(&existed); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS native_identity_pending(session TEXT PRIMARY KEY REFERENCES sessions(id), room TEXT NOT NULL, started INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS room_stop_pauses(room TEXT PRIMARY KEY, session TEXT NOT NULL REFERENCES sessions(id), started INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS usage_pending(session TEXT NOT NULL REFERENCES sessions(id), result_id TEXT NOT NULL, room TEXT NOT NULL, at INTEGER NOT NULL, event TEXT NOT NULL, PRIMARY KEY(session,result_id));
CREATE INDEX IF NOT EXISTS usage_pending_at ON usage_pending(at,session,result_id);`)
	if err == nil && existed == 0 {
		// Older builds had no durable unresolved-identity state. A bound
		// launch with no CCID might already have created a context; do not
		// silently replace it during upgrade. Untouched sessions still start
		// normally. This backfill runs once, not after confirmed unsent stops.
		_, err = tx.Exec(`INSERT OR IGNORE INTO native_identity_pending(session,room,started)
SELECT s.id,s.room,s.created FROM sessions s JOIN session_launch_identity l ON l.session=s.id WHERE s.cc IS NULL OR s.cc=''`)
	}
	return err
}

// ErrNativeIdentityPending means an earlier launch may have created a native
// context whose identity was never recorded. Only an observed native ID, or a
// confirmed launch that never handed off input, can resolve this fence.
var ErrNativeIdentityPending = errors.New("native session identity remains unresolved")

// BeginNativeLaunch commits the fence before a new native context can exist.
// Existing contexts must still match the caller's snapshot: a late ready must
// not turn an already-prepared fresh launch into an accidental replacement.
func (s *Store) BeginNativeLaunch(ctx context.Context, session core.Session) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		var cc sql.NullString
		var room string
		if err := tx.QueryRowContext(ctx, "SELECT room,cc FROM sessions WHERE id=?", session.ID).Scan(&room, &cc); err != nil {
			return err
		}
		if room != session.RoomKey || cc.String != session.CCID {
			return ErrIdentityConflict
		}
		if cc.String != "" {
			return nil
		}
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO native_identity_pending(session,room,started) VALUES(?,?,?)", session.ID, room, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return ErrNativeIdentityPending
		}
		return err
	})
}

func (s *Store) NativeIdentityPending(session string) (bool, error) {
	var n int
	err := s.DB.QueryRow("SELECT count(*) FROM native_identity_pending WHERE session=?", session).Scan(&n)
	return n != 0, err
}

// ClearUnsentNativeLaunch is only for a process whose cleanup and event drain
// are confirmed, and which was never handed a user input.
func (s *Store) ClearUnsentNativeLaunch(session string) error {
	_, err := s.DB.Exec("DELETE FROM native_identity_pending WHERE session=?", session)
	return err
}

func (s *Store) PauseRoomStop(ctx context.Context, room, session string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO room_stop_pauses(room,session,started) VALUES(?,?,?) ON CONFLICT(room) DO UPDATE SET session=excluded.session", room, session, time.Now().UnixMilli())
	return err
}

func (s *Store) PausedStopRooms(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT room FROM room_stop_pauses")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []string
	for rows.Next() {
		var room string
		if err = rows.Scan(&room); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

// FinishRoomStop cancels inputs admitted during the stop and removes its
// durable pause together. The gateway serializes admission with this commit.
func (s *Store) FinishRoomStop(ctx context.Context, room, session string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE inbox SET status='cancelled',payload=NULL WHERE session=? AND status IN ('queued','sent','consumed')", session); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM room_stop_pauses WHERE room=? AND session=?", room, session)
		return err
	})
}
