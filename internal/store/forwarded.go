package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/sean2077/feishu-bot/internal/core"
)

// ForwardedInputs are topic messages submitted to a live local native inbox.
// They are not queued gateway inputs: they never start a native process and
// they are never replayed after the terminal exits.
const InputForwarded = "forwarded"

// InsertForwarded persists a topic input before the inbox submission. It is
// idempotent for the same room and message ID.
func (s *Store) InsertForwarded(room string, m core.Message) (bool, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return false, err
	}
	result, err := s.DB.Exec("INSERT OR IGNORE INTO inbox(room,id,message,status,at) VALUES(?,?,?,?,?)", room, m.ID, string(encoded), InputForwarded, m.Time)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// Forwarded returns the stored input and its current status.
func (s *Store) Forwarded(room, id string) (core.Message, string, bool, error) {
	var raw, status string
	err := s.DB.QueryRow("SELECT message,status FROM inbox WHERE room=? AND id=?", room, id).Scan(&raw, &status)
	if errors.Is(err, sql.ErrNoRows) {
		var empty core.Message
		return empty, "", false, nil
	}
	if err != nil {
		var empty core.Message
		return empty, "", false, err
	}
	var message core.Message
	if err = json.Unmarshal([]byte(raw), &message); err != nil {
		var empty core.Message
		return empty, "", false, err
	}
	return message, status, true, nil
}

// MarkForwarded moves a forwarded or uncertain row to a terminal status. A
// done row is never reopened.
func (s *Store) MarkForwarded(room, id, status string) (bool, error) {
	result, err := s.DB.Exec("UPDATE inbox SET status=? WHERE room=? AND id=? AND status IN (?,?)", status, room, id, InputForwarded, "uncertain")
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// UnansweredForwarded lists messages still awaiting a local reply.
func (s *Store) UnansweredForwarded(room string) ([]core.Message, error) {
	rows, err := s.DB.Query("SELECT message FROM inbox WHERE room=? AND status=? ORDER BY at,id", room, InputForwarded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanForwardedMessages(rows)
}

// FailForwarded marks every unanswered forwarded message in the room as
// uncertain and returns them for one notification. It never replays them.
func (s *Store) FailForwarded(room string) ([]core.Message, error) {
	var failed []core.Message
	err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT message FROM inbox WHERE room=? AND status=? ORDER BY at,id", room, InputForwarded)
		if err != nil {
			return err
		}
		failed, err = scanForwardedMessages(rows)
		rows.Close()
		if err != nil {
			return err
		}
		if len(failed) == 0 {
			return nil
		}
		_, err = tx.Exec("UPDATE inbox SET status='uncertain' WHERE room=? AND status=?", room, InputForwarded)
		return err
	})
	return failed, err
}

func scanForwardedMessages(rows *sql.Rows) ([]core.Message, error) {
	result := []core.Message{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var message core.Message
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			return nil, err
		}
		result = append(result, message)
	}
	return result, rows.Err()
}

// ForwardedRooms lists rooms with unanswered forwarded inputs.
func (s *Store) ForwardedRooms() ([]string, error) {
	return stringsQuery(s.DB, "SELECT DISTINCT room FROM inbox WHERE status=? ORDER BY room", InputForwarded)
}
