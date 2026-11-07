package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func initializeTopicContext(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS topic_root_context(
 session TEXT PRIMARY KEY REFERENCES sessions(id), room TEXT NOT NULL,
 root TEXT NOT NULL, input_uuid TEXT NOT NULL, confirmed_at INTEGER);`)
	return err
}

// StageTopicRoot records which actual input carries the root. The caller uses
// the same transaction as queued -> sent; preparation or a successful Fetch
// alone is not evidence that the native session received any context.
func (s *Store) StageTopicRoot(tx *sql.Tx, room, session, root, uuid string) error {
	if root == "" || uuid == "" {
		return errors.New("topic root delivery needs a root and input UUID")
	}
	result, err := tx.Exec(`INSERT INTO topic_root_context(session,room,root,input_uuid)
 SELECT id,room,?,? FROM sessions WHERE id=? AND room=?
 ON CONFLICT(session) DO UPDATE SET input_uuid=excluded.input_uuid
 WHERE topic_root_context.room=excluded.room AND topic_root_context.root=excluded.root AND topic_root_context.confirmed_at IS NULL`, root, uuid, session, room)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 0 {
		return err
	}
	var found int
	if err = tx.QueryRow("SELECT 1 FROM topic_root_context WHERE room=? AND session=? AND root=?", room, session, root).Scan(&found); err != nil {
		return errors.New("topic root delivery does not belong to this session")
	}
	return nil
}

// ConfirmTopicRoot runs with a native consumption acknowledgement or a final
// result for this exact input. Its fact survives input retention and compact.
func (s *Store) ConfirmTopicRoot(tx *sql.Tx, room, session, uuid string) error {
	_, err := tx.Exec("UPDATE topic_root_context SET confirmed_at=? WHERE room=? AND session=? AND input_uuid=? AND confirmed_at IS NULL", time.Now().UnixMilli(), room, session, uuid)
	return err
}

func (s *Store) TopicRootDelivered(ctx context.Context, room, session, root string) (bool, error) {
	var found int
	err := s.DB.QueryRowContext(ctx, "SELECT 1 FROM topic_root_context WHERE room=? AND session=? AND root=? AND confirmed_at IS NOT NULL", room, session, root).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
