package store

import (
	"database/sql"
	"errors"
	"time"
)

// StatusCard returns the message ID of the room's persistent status card, or
// an empty string when no card has been created yet.
func (s *Store) StatusCard(room string) (string, error) {
	var message string
	err := s.DB.QueryRow("SELECT message FROM status_cards WHERE room=?", room).Scan(&message)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return message, err
}

func (s *Store) SaveStatusCard(room, message string) error {
	_, err := s.DB.Exec(`INSERT INTO status_cards(room,message,updated_at) VALUES(?,?,?)
ON CONFLICT(room) DO UPDATE SET message=excluded.message,updated_at=excluded.updated_at`, room, message, time.Now().UnixMilli())
	return err
}
