package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sean2077/feishu-bot/internal/core"
)

var (
	ErrInputNotFound  = errors.New("input not found in this chat")
	ErrInputNotOwned  = errors.New("only the input author or a maintainer can cancel it")
	ErrInputHandedOff = errors.New("input is no longer proven unsubmitted; it cannot be recalled")
	ErrInputInvalid   = errors.New("input ownership or batch binding cannot be verified")
)

type InputRecord struct {
	ID              string        `json:"id"`
	MessageID       string        `json:"messageId"`
	Session         string        `json:"session"`
	Sender          string        `json:"sender"`
	Status          string        `json:"status"`
	At              int64         `json:"at"`
	Text            string        `json:"text,omitempty"`
	BatchID         string        `json:"batchId,omitempty"`
	BatchDeadline   int64         `json:"batchDeadline,omitempty"`
	BatchClosed     bool          `json:"batchClosed,omitempty"`
	Command         string        `json:"command,omitempty"`
	SupplementTo    string        `json:"supplementTo,omitempty"`
	VerifiedPayload bool          `json:"verifiedPayload"`
	Delivery        core.Delivery `json:"-"`
}

type inputScanner interface{ Scan(...any) error }

const inputColumns = "uuid,id,session,message,status,at,payload"

func scanInput(row inputScanner, room string) (InputRecord, error) {
	var v InputRecord
	var uuid, session, payload sql.NullString
	var raw string
	if err := row.Scan(&uuid, &v.MessageID, &session, &raw, &v.Status, &v.At, &payload); err != nil {
		return v, err
	}
	v.ID, v.Session = uuid.String, session.String
	if v.ID == "" {
		v.ID = v.MessageID
	}
	var m core.Message
	if json.Unmarshal([]byte(raw), &m) == nil {
		v.Sender, v.Text = m.SenderID, m.Text
	}
	if payload.Valid && json.Unmarshal([]byte(payload.String), &v.Delivery) == nil {
		d := v.Delivery
		v.VerifiedPayload = uuid.Valid && uuid.String != "" && d.UUID == uuid.String && d.RoomKey == room && d.SessionID == session.String && d.Message.ID == v.MessageID && d.Message.SenderID != "" && d.Message.SenderID == v.Sender
		if d.ImageBatch != nil {
			v.BatchID = d.ImageBatch.ID
			v.BatchDeadline = d.ImageBatch.Deadline
			v.BatchClosed = d.ImageBatch.Closed
		}
		if d.Command != nil {
			v.Command = d.Command.Name
		}
		v.SupplementTo = d.SupplementTo
	}
	return v, nil
}

// Inputs is bounded and newest-first. It does not claim that a queued row is
// currently cancellable: only the dispatch owner can establish that boundary.
func (s *Store) Inputs(ctx context.Context, room string, limit int) ([]InputRecord, bool, error) {
	limit = min(max(limit, 1), 100)
	rows, err := s.DB.QueryContext(ctx, "SELECT "+inputColumns+" FROM inbox WHERE room=? AND status IN ('queued','sent','consumed','uncertain') ORDER BY at DESC,rowid DESC LIMIT ?", room, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []InputRecord{}
	for rows.Next() {
		v, err := scanInput(rows, room)
		if err != nil {
			return nil, false, err
		}
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

// CancelQueuedInput must be called while holding both the room input and
// dispatch locks. A queued database row alone is not proof that a concurrent
// dispatch has not begun preparing/writing it. No transport/native call belongs
// in this transaction; it atomically preserves the whole image batch decision.
func (s *Store) CancelQueuedInput(ctx context.Context, room, id, actor string, maintainer bool, now int64) ([]InputRecord, error) {
	var cancelled []InputRecord
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+inputColumns+" FROM inbox WHERE room=? AND (uuid=? OR id=?) LIMIT 2", room, id, id)
		if err != nil {
			return err
		}
		var matches []InputRecord
		for rows.Next() {
			v, e := scanInput(rows, room)
			if e != nil {
				rows.Close()
				return e
			}
			matches = append(matches, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return ErrInputNotFound
		}
		if len(matches) != 1 {
			return ErrInputInvalid
		}
		first := matches[0]
		if !maintainer && (first.Sender == "" || first.Sender != actor) {
			return ErrInputNotOwned
		}
		if first.Status != "queued" {
			return ErrInputHandedOff
		}
		if !first.VerifiedPayload || (first.Delivery.ImageBatch != nil && first.BatchID == "") {
			return ErrInputInvalid
		}
		members := []InputRecord{first}
		if first.BatchID != "" {
			rows, err = tx.QueryContext(ctx, "SELECT "+inputColumns+" FROM inbox WHERE room=? AND session=? AND json_extract(CASE WHEN json_valid(payload) THEN payload ELSE '{}' END,'$.imageBatch.id')=? ORDER BY rowid LIMIT 101", room, first.Session, first.BatchID)
			if err != nil {
				return err
			}
			members = nil
			for rows.Next() {
				v, e := scanInput(rows, room)
				if e != nil {
					rows.Close()
					return e
				}
				members = append(members, v)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(members) == 0 || len(members) > 100 {
				return ErrInputInvalid
			}
			for _, v := range members {
				if !v.VerifiedPayload || v.Sender != first.Sender || v.Session != first.Session || v.Delivery.Message.ReplyTo != first.Delivery.Message.ReplyTo || v.Delivery.Message.ThreadID != first.Delivery.Message.ThreadID || v.Command != "" || v.SupplementTo != "" {
					return ErrInputInvalid
				}
				if !v.Delivery.Message.ImageOnly || len(v.Delivery.Message.Resources) == 0 {
					return ErrInputInvalid
				}
				for _, resource := range v.Delivery.Message.Resources {
					if resource.Kind != "image" {
						return ErrInputInvalid
					}
				}
				if v.Status != "queued" {
					return ErrInputHandedOff
				}
			}
		}
		for _, v := range members {
			res, err := tx.ExecContext(ctx, "UPDATE inbox SET status='cancelled',payload=NULL WHERE room=? AND id=? AND uuid=? AND status='queued'", room, v.MessageID, v.ID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrInputHandedOff
			}
			detail, _ := json.Marshal(core.Args{"messageId": v.MessageID, "sender": v.Sender, "batchId": v.BatchID, "proof": "dispatch_excluded_before_handoff"})
			if _, err = tx.ExecContext(ctx, `INSERT INTO operator_decisions(kind,id,room,session,actor,at,decision,detail) VALUES('input',?,?,?,?,?,'cancel',?)`, v.ID, room, v.Session, actor, now, string(detail)); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE receipts SET desired='clear',at=? WHERE id=? AND room=? AND session=?", now, v.MessageID, room, v.Session); err != nil {
				return err
			}
			cancelled = append(cancelled, v)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cancel input: %w", err)
	}
	return cancelled, nil
}
