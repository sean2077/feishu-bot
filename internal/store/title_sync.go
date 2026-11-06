package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func initializeTitleSync(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS session_title_sync(session TEXT PRIMARY KEY REFERENCES sessions(id), room TEXT NOT NULL, desired TEXT NOT NULL, revision TEXT NOT NULL, confirmed TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, updated INTEGER NOT NULL)`)
	return err
}

// SessionTitleSync keeps the saved intent separate from the last native ACK.
// Native titles are display metadata; they never participate in /resume lookup.
type SessionTitleSync struct {
	Desired, Revision, Confirmed, State string
	Updated                             int64
}

func (s *Store) TitleSync(room, session string) (SessionTitleSync, error) {
	var title SessionTitleSync
	err := s.DB.QueryRow(`SELECT coalesce(t.desired,''),coalesce(t.revision,''),coalesce(t.confirmed,''),coalesce(t.state,'not_requested'),coalesce(t.updated,0) FROM sessions s LEFT JOIN session_title_sync t ON t.session=s.id AND t.room=s.room WHERE s.room=? AND s.id=?`, room, session).Scan(&title.Desired, &title.Revision, &title.Confirmed, &title.State, &title.Updated)
	return title, err
}

// PrepareTitleSync chooses from the current durable preferences in the same
// transaction that records the revision. A delayed first-message derivation
// therefore cannot overwrite a newer /rename. refresh starts a new attempt for
// an explicit rename or a newly started native worker, never a retry loop.
func (s *Store) PrepareTitleSync(room, session, fallback string, refresh bool) (SessionTitleSync, error) {
	var title SessionTitleSync
	err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		title, err = prepareTitleSync(tx, room, session, fallback, refresh)
		return err
	})
	return title, err
}

func prepareTitleSync(tx *sql.Tx, room, session, fallback string, refresh bool) (SessionTitleSync, error) {
	var name, automatic string
	if err := tx.QueryRow(`SELECT coalesce(p.name,''),coalesce(p.native_title,'') FROM sessions s LEFT JOIN session_preferences p ON p.session=s.id AND p.room=s.room WHERE s.room=? AND s.id=? AND s.state='open'`, room, session).Scan(&name, &automatic); err != nil {
		return SessionTitleSync{}, err
	}
	if automatic == "" && fallback != "" {
		if _, err := tx.Exec(`INSERT INTO session_preferences(session,room,native_title) VALUES(?,?,?) ON CONFLICT(session) DO UPDATE SET native_title=excluded.native_title WHERE session_preferences.room=excluded.room AND session_preferences.native_title=''`, session, room, fallback); err != nil {
			return SessionTitleSync{}, err
		}
		automatic = fallback
	}
	desired := name
	if desired == "" {
		desired = automatic
	}
	// Match the native control's whitespace normalization while preserving the
	// separately stored chat-local name used by /resume.
	desired = strings.Join(strings.Fields(desired), " ")
	var old SessionTitleSync
	err := tx.QueryRow(`SELECT desired,revision,confirmed,state,updated FROM session_title_sync WHERE room=? AND session=?`, room, session).Scan(&old.Desired, &old.Revision, &old.Confirmed, &old.State, &old.Updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SessionTitleSync{}, err
	}
	if err == nil && old.Desired == desired && !refresh {
		return old, nil
	}
	title := SessionTitleSync{Desired: desired, Revision: core.NewID(), Confirmed: old.Confirmed, State: "pending", Updated: time.Now().UnixMilli()}
	if desired == "" {
		title.State = "not_required"
	}
	_, err = tx.Exec(`INSERT INTO session_title_sync(session,room,desired,revision,confirmed,state,updated) VALUES(?,?,?,?,?,?,?) ON CONFLICT(session) DO UPDATE SET desired=excluded.desired,revision=excluded.revision,state=excluded.state,updated=excluded.updated WHERE session_title_sync.room=excluded.room`, session, room, title.Desired, title.Revision, title.Confirmed, title.State, title.Updated)
	return title, err
}

// FinishTitleSync ignores acknowledgements from older attempts. A rename and
// its desired revision are committed together, so the previous response cannot
// report the new title as confirmed even in the command-to-worker gap.
func (s *Store) FinishTitleSync(room, session, revision, state string) (bool, error) {
	switch state {
	case "confirmed", string(core.ControlNotSent), string(core.ControlRejected), string(core.ControlUnconfirmed):
	default:
		return false, errors.New("invalid native title outcome")
	}
	result, err := s.DB.Exec(`UPDATE session_title_sync SET state=?,confirmed=CASE WHEN ?='confirmed' THEN desired ELSE confirmed END,updated=? WHERE room=? AND session=? AND revision=? AND state='pending' AND EXISTS(SELECT 1 FROM sessions WHERE id=? AND room=? AND state='open')`, state, state, time.Now().UnixMilli(), room, session, revision, session, room)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
