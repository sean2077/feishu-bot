package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"modernc.org/sqlite"
)

// Adoption states are durable. A topic room is routable only in the open
// state; pending and uncertain records are evidence and status, never a
// request to launch another native process.
const (
	AdoptedPending   = "pending"
	AdoptedOpen      = "open"
	AdoptedUncertain = "uncertain"
)

// ErrNativeClaimed reports that a native session ID already belongs to a
// gateway session or another adoption.
var ErrNativeClaimed = errors.New("native session is already claimed")

// SQLite extended constraint codes that prove one row already owns the key.
// modernc.org/sqlite returns extended codes from Error.Code().
const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

func nativeClaimConflictCode(code int) bool {
	return code == sqliteConstraintPrimaryKey || code == sqliteConstraintUnique
}

func nativeClaimConflict(err error) bool {
	var failure *sqlite.Error
	return errors.As(err, &failure) && nativeClaimConflictCode(failure.Code())
}

// AdoptedTopic is one locally adopted Claude Code session bound to a Feishu
// DM topic. Room, Root, Session and Thread are empty until the corresponding
// durable step completes.
type AdoptedTopic struct {
	User          string
	Room          string
	Session       string
	Native        string
	Thread        string
	Root          string
	Work          string
	SessionName   string
	Title         string
	HolderPID     int
	HolderStarted int64
	State         string
	Created       int64
	Updated       int64
}

const adoptedCols = "native,user,coalesce(room,''),coalesce(session,''),thread,coalesce(root,''),work,coalesce(session_name,''),coalesce(title,''),holder_pid,holder_started,state,created,updated"

func adoptedRow(r scanner) (AdoptedTopic, error) {
	var a AdoptedTopic
	err := r.Scan(&a.Native, &a.User, &a.Room, &a.Session, &a.Thread, &a.Root, &a.Work, &a.SessionName, &a.Title, &a.HolderPID, &a.HolderStarted, &a.State, &a.Created, &a.Updated)
	return a, err
}

type rowQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func adoptedBy(q rowQueryer, column, value string) (AdoptedTopic, bool, error) {
	a, err := adoptedRow(q.QueryRow("SELECT "+adoptedCols+" FROM adopted_topics WHERE "+column+"=?", value))
	if errors.Is(err, sql.ErrNoRows) {
		return AdoptedTopic{}, false, nil
	}
	return a, err == nil, err
}

// AdoptedTopicByNative returns the adoption record for one native session ID.
func (s *Store) AdoptedTopicByNative(native string) (AdoptedTopic, bool, error) {
	return adoptedBy(s.DB, "native", native)
}

// AdoptedTopicByRoom returns the adoption record for one DM topic room key.
func (s *Store) AdoptedTopicByRoom(room string) (AdoptedTopic, bool, error) {
	return adoptedBy(s.DB, "room", room)
}

// AdoptedTopicByThread returns the adoption record for one Feishu thread ID.
func (s *Store) AdoptedTopicByThread(thread string) (AdoptedTopic, bool, error) {
	if thread == "" {
		return AdoptedTopic{}, false, nil
	}
	return adoptedBy(s.DB, "thread", thread)
}

// AdoptedTopicByRoot returns the adoption record for one root message ID.
func (s *Store) AdoptedTopicByRoot(root string) (AdoptedTopic, bool, error) {
	if root == "" {
		return AdoptedTopic{}, false, nil
	}
	return adoptedBy(s.DB, "root", root)
}

// AdoptedTopicByRoomDB is the transport-side variant for callers that hold the
// shared *sql.DB rather than a Store.
func AdoptedTopicByRoomDB(db *sql.DB, room string) (AdoptedTopic, bool, error) {
	if db == nil || room == "" {
		return AdoptedTopic{}, false, nil
	}
	return adoptedBy(db, "room", room)
}

// AdoptedTopicByThreadDB is the transport-side thread lookup.
func AdoptedTopicByThreadDB(db *sql.DB, thread string) (AdoptedTopic, bool, error) {
	if db == nil || thread == "" {
		return AdoptedTopic{}, false, nil
	}
	return adoptedBy(db, "thread", thread)
}

// AdoptedTopicByRootDB is the transport-side root lookup.
func AdoptedTopicByRootDB(db *sql.DB, root string) (AdoptedTopic, bool, error) {
	if db == nil || root == "" {
		return AdoptedTopic{}, false, nil
	}
	return adoptedBy(db, "root", root)
}

// CurrentSession returns the room's open session without creating one.
func (s *Store) CurrentSession(room string) (core.Session, bool, error) {
	r, err := sessionRow(s.DB.QueryRow("SELECT "+sessionCols+" FROM sessions WHERE room=? AND state='open'", room))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Session{}, false, nil
	}
	return r, err == nil, err
}

// AdoptedTopics lists every adoption record, oldest first.
func (s *Store) AdoptedTopics() ([]AdoptedTopic, error) {
	rows, err := s.DB.Query("SELECT " + adoptedCols + " FROM adopted_topics ORDER BY created,native")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AdoptedTopic{}
	for rows.Next() {
		a, e := adoptedRow(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// NativeClaimed reports whether native is already recorded as any gateway
// session's native identity or as an adoption.
func (s *Store) NativeClaimed(native string) (bool, error) {
	var n int
	err := s.DB.QueryRow("SELECT (SELECT count(*) FROM sessions WHERE cc=?)+(SELECT count(*) FROM adopted_topics WHERE native=?)", native, native).Scan(&n)
	return n > 0, err
}

// BeginAdoption persists the intent before any external write. It is
// idempotent: an existing record is returned unchanged.
func (s *Store) BeginAdoption(native, user, work string, holderPID int, holderStarted int64) (AdoptedTopic, bool, error) {
	now := time.Now().UnixMilli()
	var result AdoptedTopic
	created := false
	err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		result, err = adoptedRow(tx.QueryRow("SELECT "+adoptedCols+" FROM adopted_topics WHERE native=?", native))
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var claimed int
		if err = tx.QueryRow("SELECT count(*) FROM sessions WHERE cc=?", native).Scan(&claimed); err != nil {
			return err
		}
		if claimed > 0 {
			return ErrNativeClaimed
		}
		if _, err = tx.Exec("INSERT INTO adopted_topics(native,user,room,session,thread,root,work,holder_pid,holder_started,state,created,updated) VALUES(?,?,NULL,NULL,'',NULL,?,?,?,?,?,?)",
			native, user, work, holderPID, holderStarted, AdoptedPending, now, now); err != nil {
			// Only a primary-key or unique-constraint conflict means another
			// adoption or gateway session already owns this native identity.
			// Disk, locking, trigger and schema failures must reach the caller
			// unchanged. modernc.org/sqlite reports extended result codes here.
			if nativeClaimConflict(err) {
				return ErrNativeClaimed
			}
			return err
		}
		result, err = adoptedRow(tx.QueryRow("SELECT "+adoptedCols+" FROM adopted_topics WHERE native=?", native))
		created = err == nil
		return err
	})
	return result, created, err
}

// SetAdoptionIdentity records the local session display name and root card
// title with the pending intent, before any external write.
func (s *Store) SetAdoptionIdentity(native, sessionName, title string) error {
	_, err := s.DB.Exec("UPDATE adopted_topics SET session_name=?,title=?,updated=? WHERE native=?",
		sessionName, title, time.Now().UnixMilli(), native)
	return err
}

// SetAdoptionSessionName updates the display name shown for one adoption. It is
// used when a later attach or notify observes a renamed local session.
func (s *Store) SetAdoptionSessionName(native, sessionName string) error {
	_, err := s.DB.Exec("UPDATE adopted_topics SET session_name=?,updated=? WHERE native=?",
		sessionName, time.Now().UnixMilli(), native)
	return err
}

// SetAdoptionRootEvidence records a delivered root message and its room before
// the session transaction. It is idempotent for the same evidence and refuses
// a conflicting record. Keeping the evidence lets an interrupted adoption
// still match topic replies and report its uncertain state.
func (s *Store) SetAdoptionRootEvidence(native, room, root string) error {
	if native == "" || room == "" || root == "" {
		return errors.New("incomplete adoption root evidence")
	}
	result, err := s.DB.Exec("UPDATE adopted_topics SET room=?,root=?,updated=? WHERE native=? AND state=? AND room IS NULL AND root IS NULL",
		room, root, time.Now().UnixMilli(), native, AdoptedPending)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var state string
	var existingRoom, existingRoot sql.NullString
	if err = s.DB.QueryRow("SELECT state,room,root FROM adopted_topics WHERE native=?", native).Scan(&state, &existingRoom, &existingRoot); err != nil {
		return err
	}
	if state == AdoptedPending && existingRoom.String == room && existingRoot.String == root {
		return nil
	}
	return errors.New("adoption root evidence conflicts with the existing record")
}

// BindAdoptionRoot records the delivered root message and creates the gateway
// session and launch identity in the same transaction. It runs only after the
// root card was sent successfully. A room/root evidence row written before the
// transaction is accepted when it matches; a conflicting row is refused.
func (s *Store) BindAdoptionRoot(native, room, root, session, configDir string, configOverride bool, created int64) error {
	if native == "" || room == "" || root == "" || session == "" || configDir == "" {
		return errors.New("incomplete adoption root")
	}
	if created <= 0 {
		created = time.Now().UnixMilli()
	}
	override := 0
	if configOverride {
		override = 1
	}
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var work, state string
		var existingRoom, existingRoot sql.NullString
		var existingSession sql.NullString
		if err := tx.QueryRow("SELECT work,state,room,root,session FROM adopted_topics WHERE native=?", native).Scan(&work, &state, &existingRoom, &existingRoot, &existingSession); err != nil {
			return err
		}
		if state != AdoptedPending || existingSession.Valid {
			return errors.New("adoption is not awaiting a root")
		}
		if existingRoom.Valid && existingRoom.String != room {
			return errors.New("adoption root evidence does not match")
		}
		if existingRoot.Valid && existingRoot.String != root {
			return errors.New("adoption root evidence does not match")
		}
		var claimed int
		if err := tx.QueryRow("SELECT count(*) FROM sessions WHERE cc=?", native).Scan(&claimed); err != nil {
			return err
		}
		if claimed > 0 {
			return ErrNativeClaimed
		}
		if _, err := tx.Exec("INSERT INTO sessions(id,room,cc,state,created) VALUES(?,?,?,'open',?)", session, room, native, created); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO session_launch_identity(session,work,native_config,config_dir,config_override) VALUES(?,?,'inherit',?,?)", session, work, configDir, override); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO usage_coverage(session,room,started,prior_unknown) VALUES(?,?,?,1)", session, room, created); err != nil {
			return err
		}
		result, err := tx.Exec("UPDATE adopted_topics SET room=?,root=?,session=?,updated=? WHERE native=? AND state=? AND session IS NULL AND (room IS NULL OR room=?) AND (root IS NULL OR root=?)",
			room, root, session, time.Now().UnixMilli(), native, AdoptedPending, room, root)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return errors.New("adoption root was not recorded")
		}
		return nil
	})
}

// OpenAdoption records the Feishu thread ID and makes the room routable.
func (s *Store) OpenAdoption(native, thread string) error {
	if native == "" || thread == "" {
		return errors.New("incomplete adoption topic")
	}
	result, err := s.DB.Exec("UPDATE adopted_topics SET thread=?,state=?,updated=? WHERE native=? AND state=? AND room IS NOT NULL AND root IS NOT NULL AND thread=''",
		thread, AdoptedOpen, time.Now().UnixMilli(), native, AdoptedPending)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("adoption is not awaiting a thread")
	}
	return nil
}

// MarkAdoptionUncertain preserves an interrupted adoption. It never downgrades
// an open topic.
func (s *Store) MarkAdoptionUncertain(native string) error {
	_, err := s.DB.Exec("UPDATE adopted_topics SET state=?,updated=? WHERE native=? AND state=?", AdoptedUncertain, time.Now().UnixMilli(), native, AdoptedPending)
	return err
}

// DeleteUnsentAdoption removes a pending record only when no external write
// can have happened: room and root are still absent.
func (s *Store) DeleteUnsentAdoption(native string) error {
	_, err := s.DB.Exec("DELETE FROM adopted_topics WHERE native=? AND state=? AND room IS NULL AND root IS NULL AND session IS NULL", native, AdoptedPending)
	return err
}

// ExpirePendingAdoptions moves abandoned intents to uncertain so a retry never
// replays the external write.
func (s *Store) ExpirePendingAdoptions(before int64, now int64) error {
	_, err := s.DB.Exec("UPDATE adopted_topics SET state=?,updated=? WHERE state=? AND created<?", AdoptedUncertain, now, AdoptedPending, before)
	return err
}

// UpdateAdoptionHolder records the local terminal process currently owning
// the adopted native session.
func (s *Store) UpdateAdoptionHolder(native string, pid int, started int64) error {
	_, err := s.DB.Exec("UPDATE adopted_topics SET holder_pid=?,holder_started=?,updated=? WHERE native=? AND state IN (?,?)",
		pid, started, time.Now().UnixMilli(), native, AdoptedPending, AdoptedOpen)
	return err
}

// ClearAdoptionHolder releases a holder only when it still matches the
// recorded process identity.
func (s *Store) ClearAdoptionHolder(native string, pid int, started int64) (bool, error) {
	result, err := s.DB.Exec("UPDATE adopted_topics SET holder_pid=0,holder_started=0,updated=? WHERE native=? AND holder_pid=? AND holder_started=?",
		time.Now().UnixMilli(), native, pid, started)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// AuditDB appends an audit row through an existing database handle. Transport
// adapters use it for rejected inbound scopes before the gateway sees them.
func AuditDB(db *sql.DB, room, event string, detail core.Args) error {
	if detail == nil {
		detail = core.Args{}
	}
	j, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO audit(at,room,event,detail) VALUES(?,?,?,?)", time.Now().UnixMilli(), room, event, string(j))
	return err
}
