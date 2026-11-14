// Package store preserves the v0.2 SQLite schema and room/session identities.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, room TEXT NOT NULL, cc TEXT, state TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS session_launch_identity(session TEXT PRIMARY KEY REFERENCES sessions(id), work TEXT NOT NULL, native_config TEXT NOT NULL, config_dir TEXT NOT NULL, config_override INTEGER NOT NULL DEFAULT 1);
CREATE UNIQUE INDEX IF NOT EXISTS one_active ON sessions(room) WHERE state='open';
CREATE TABLE IF NOT EXISTS inbox(room TEXT NOT NULL, id TEXT NOT NULL, message TEXT NOT NULL, status TEXT NOT NULL, at INTEGER NOT NULL, uuid TEXT, session TEXT, payload TEXT, PRIMARY KEY(room,id));
CREATE INDEX IF NOT EXISTS inbox_status ON inbox(room,status,at);
CREATE TABLE IF NOT EXISTS outputs(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS feishu_cards(message TEXT PRIMARY KEY, card TEXT NOT NULL, room TEXT NOT NULL, sequence INTEGER NOT NULL DEFAULT 0, snapshot TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS status_cards(room TEXT PRIMARY KEY, message TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS audit(seq INTEGER PRIMARY KEY, at INTEGER NOT NULL, room TEXT NOT NULL, event TEXT NOT NULL, detail TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, kind TEXT NOT NULL, digest TEXT NOT NULL, state TEXT NOT NULL, expires INTEGER NOT NULL, actor TEXT);
CREATE TABLE IF NOT EXISTS outbox(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, text TEXT NOT NULL, target TEXT, reply_to TEXT, thread_id TEXT, success INTEGER NOT NULL, state TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS action_runs(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, action TEXT NOT NULL, digest TEXT NOT NULL, state TEXT NOT NULL, at INTEGER NOT NULL, approval TEXT);
CREATE TABLE IF NOT EXISTS feedback(id TEXT PRIMARY KEY, room TEXT NOT NULL, project TEXT NOT NULL, session TEXT NOT NULL, native_session TEXT, output TEXT NOT NULL, message TEXT NOT NULL, user TEXT NOT NULL, value TEXT NOT NULL, question TEXT NOT NULL, answer_summary TEXT NOT NULL, at INTEGER NOT NULL, UNIQUE(output,user));
CREATE TABLE IF NOT EXISTS receipts(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, desired TEXT NOT NULL, applied TEXT, at INTEGER NOT NULL, sender TEXT);
CREATE TABLE IF NOT EXISTS context_refs(room TEXT NOT NULL, id TEXT NOT NULL, message TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(room,id));
CREATE TABLE IF NOT EXISTS usage_coverage(session TEXT PRIMARY KEY, room TEXT NOT NULL, started INTEGER NOT NULL, prior_unknown INTEGER NOT NULL, unidentified INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS usage_results(session TEXT NOT NULL, result_id TEXT NOT NULL, room TEXT NOT NULL, at INTEGER NOT NULL, kind TEXT NOT NULL, success INTEGER NOT NULL, models TEXT NOT NULL,
 input_tokens INTEGER, output_tokens INTEGER, cache_read_input_tokens INTEGER, cache_creation_input_tokens INTEGER, PRIMARY KEY(session,result_id));
CREATE INDEX IF NOT EXISTS usage_room_session ON usage_results(room,session,at);
CREATE TABLE IF NOT EXISTS adopted_topics(native TEXT PRIMARY KEY, room TEXT UNIQUE, session TEXT REFERENCES sessions(id), thread TEXT NOT NULL DEFAULT '', root TEXT UNIQUE, work TEXT NOT NULL, holder_pid INTEGER NOT NULL DEFAULT 0, holder_started INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, created INTEGER NOT NULL, updated INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS adopted_topics_thread ON adopted_topics(thread);
CREATE INDEX IF NOT EXISTS adopted_topics_state ON adopted_topics(state,updated);
`

type Store struct {
	DB     *sql.DB
	closed atomic.Bool
}
type Recovery struct {
	Rooms, RequestRooms []string
	Requests            int64
}

func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
			return nil, err
		}
		dsn = sqliteFileURI(filepath.ToSlash(abs))
	}
	dsn += "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err = s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// A Windows drive must remain in the URI path, not become its authority.
// file://C:/state is an invalid SQLite authority; file:///C:/state is a file.
func sqliteFileURI(path string) string {
	if len(path) >= 2 && path[1] == ':' {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func New(path string) (*Store, error) { return Open(path) }

// OpenReadOnly opens an existing state database without running migrations or
// taking the service lock. WAL readers can run while the gateway is writing.
func OpenReadOnly(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("existing gateway state is required for feedback export")
	}
	dsn := sqliteFileURI(filepath.ToSlash(abs)) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}
func (s *Store) IsOpen() bool { return !s.closed.Load() }
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.DB.Close()
}
func (s *Store) initialize() error {
	var version int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 5 {
		return fmt.Errorf("state schema %d is newer than supported schema 5", version)
	}
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		if err := initializeConvenience(tx); err != nil {
			return err
		}
		if err := initializeRuntimeState(tx); err != nil {
			return err
		}
		for _, col := range []struct{ table, name, decl string }{
			{"receipts", "sender", "TEXT"}, {"session_launch_identity", "config_override", "INTEGER NOT NULL DEFAULT 1"},
			{"outbox", "card", "TEXT"}, {"outbox", "origins", "TEXT NOT NULL DEFAULT '[]'"}, {"outbox", "file_name", "TEXT"}, {"outbox", "file_path", "TEXT"},
			{"outbox", "delivery_key", "TEXT"}, {"outbox", "initial_card", "TEXT"}, {"outbox", "retry_count", "INTEGER NOT NULL DEFAULT 0"}, {"outbox", "retry_after", "INTEGER NOT NULL DEFAULT 0"},
			{"outbox", "file_target", "TEXT"}, {"outbox", "file_state", "TEXT NOT NULL DEFAULT 'none'"}, {"outbox", "delivery_error", "TEXT"}, {"outbox", "kind", "TEXT NOT NULL DEFAULT 'reply'"},
			{"session_preferences", "native_title", "TEXT NOT NULL DEFAULT ''"},
			{"action_runs", "requester", "TEXT"}, {"action_runs", "request_context", "TEXT"},
			{"action_runs", "request", "TEXT"}, {"action_runs", "approver", "TEXT"}, {"action_runs", "taint_request", "TEXT"}, {"action_runs", "approval", "TEXT"},
			{"requests", "requester", "TEXT"}, {"requests", "request_context", "TEXT"},
		} {
			rows, err := tx.Query("PRAGMA table_info(" + col.table + ")")
			if err != nil {
				return err
			}
			found := false
			for rows.Next() {
				var cid, notnull, pk int
				var name, typ string
				var def any
				if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
					rows.Close()
					return err
				}
				if name == col.name {
					found = true
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if !found {
				if _, err = tx.Exec("ALTER TABLE " + col.table + " ADD COLUMN " + col.name + " " + col.decl); err != nil {
					return err
				}
			}
		}
		// Retention rechecks each candidate against the durable outbox inside
		// its transaction. Index all three reference forms rather than copying
		// or scanning every outstanding output for each card.
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS outbox_target_state ON outbox(target,state);
CREATE INDEX IF NOT EXISTS outbox_file_target_state ON outbox(file_target,state);
CREATE INDEX IF NOT EXISTS outbox_delivery_key_state ON outbox(delivery_key,state);
CREATE INDEX IF NOT EXISTS outbox_kind_at ON outbox(kind,at);`); err != nil {
			return err
		}
		if err := initializeMaintenance(tx); err != nil {
			return err
		}
		if err := initializeSessionCatalog(tx); err != nil {
			return err
		}
		if err := initializeTopicContext(tx); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT OR IGNORE INTO usage_coverage(session,room,started,prior_unknown) SELECT id,room,?,1 FROM sessions", time.Now().UnixMilli())
		if err != nil {
			return err
		}
		_, err = tx.Exec("PRAGMA user_version=5")
		return err
	})
}
func (s *Store) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type scanner interface{ Scan(...any) error }

func sessionRow(r scanner) (core.Session, error) {
	var s core.Session
	var cc sql.NullString
	err := r.Scan(&s.ID, &s.RoomKey, &cc, &s.State, &s.CreatedAt)
	s.CCID = cc.String
	return s, err
}

const sessionCols = "id,room,cc,state,created"

func (s *Store) Session(room string) (core.Session, error) {
	var result core.Session
	err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		r, err := sessionRow(tx.QueryRow("SELECT "+sessionCols+" FROM sessions WHERE room=? AND state='open'", room))
		if err == sql.ErrNoRows {
			r, err = newSession(tx, room)
		}
		result = r
		return err
	})
	return result, err
}
func newSession(tx *sql.Tx, room string) (core.Session, error) {
	s := core.Session{ID: core.NewID(), RoomKey: room, State: "open", CreatedAt: time.Now().UnixMilli()}
	if _, err := tx.Exec("UPDATE sessions SET state='archived' WHERE room=? AND state='open'", room); err != nil {
		return s, err
	}
	if _, err := tx.Exec("INSERT INTO sessions(id,room,cc,state,created) VALUES(?,?,NULL,?,?)", s.ID, room, s.State, s.CreatedAt); err != nil {
		return s, err
	}
	_, err := tx.Exec("INSERT INTO usage_coverage(session,room,started,prior_unknown) VALUES(?,?,?,0)", s.ID, room, s.CreatedAt)
	return s, err
}
func (s *Store) NewSession(room string) (core.Session, error) {
	var result core.Session
	err := s.Transaction(context.Background(), func(tx *sql.Tx) error { var e error; result, e = newSession(tx, room); return e })
	return result, err
}
func (s *Store) ListSessions(room string) ([]core.Session, error) {
	rows, err := s.DB.Query("SELECT "+sessionCols+" FROM sessions WHERE room=? AND (state='open' OR id IN (SELECT id FROM sessions WHERE room=? ORDER BY created DESC,id DESC LIMIT 20)) ORDER BY created DESC,id DESC", room, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []core.Session{}
	for rows.Next() {
		v, e := sessionRow(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) Resume(room, id string) (core.Session, error) {
	var result core.Session
	err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		r, e := sessionRow(tx.QueryRow("SELECT "+sessionCols+" FROM sessions WHERE room=? AND id=?", room, id))
		if e != nil {
			return errors.New("会话不属于当前聊天")
		}
		if _, e = tx.Exec("UPDATE sessions SET state='archived' WHERE room=? AND state='open'", room); e != nil {
			return e
		}
		if _, e = tx.Exec("UPDATE sessions SET state='open' WHERE room=? AND id=?", room, id); e != nil {
			return e
		}
		r.State = "open"
		result = r
		return nil
	})
	return result, err
}
func (s *Store) SetCC(id, cc string) error {
	if cc == "" {
		return errors.New("native session id cannot be cleared")
	}
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE sessions SET cc=? WHERE id=? AND (cc IS NULL OR cc='' OR cc=?)", cc, id, cc)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrIdentityConflict
		}
		_, err = tx.Exec("DELETE FROM native_identity_pending WHERE session=?", id)
		return err
	})
}

// ErrIdentityConflict reports that the session row already holds a different
// native ID (or no longer exists). Retrying cannot resolve it.
var ErrIdentityConflict = errors.New("native session identity changed or session no longer exists")

// ErrLaunchIdentityChanged requires an explicit configuration or session
// choice. Retrying must not move the existing native history to another cwd.
var ErrLaunchIdentityChanged = errors.New("会话启动身份与当前配置不匹配。请恢复原配置，或显式使用 /new；原会话已保留")

func (s *Store) SetCc(id, cc string) error { return s.SetCC(id, cc) }
func samePath(a, b string) bool {
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
func (s *Store) BindLaunchIdentity(session core.Session, w core.Workspace, explicitCWD bool) error {
	mode := w.NativeConfig
	if mode == "" {
		mode = "isolated"
	}
	configDir := w.ConfigDir
	if configDir == "" {
		configDir = filepath.Join(w.Home, ".claude")
	}
	// Isolated launches always set CLAUDE_CONFIG_DIR. In the previous schema
	// an omitted override meant true; only inherited profiles can leave it unset.
	override := 1
	if mode == "inherit" && !w.ConfigDirOverride {
		override = 0
	}
	if (mode != "isolated" && mode != "inherit") || w.Work == "" || w.Home == "" {
		return errors.New("incomplete native launch identity")
	}
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var room string
		var cc sql.NullString
		if err := tx.QueryRow("SELECT room,cc FROM sessions WHERE id=?", session.ID).Scan(&room, &cc); err != nil || room != session.RoomKey {
			return errors.New("会话不属于当前聊天")
		}
		var work, savedMode, dir string
		var savedOverride int
		err := tx.QueryRow("SELECT work,native_config,config_dir,config_override FROM session_launch_identity WHERE session=?", session.ID).Scan(&work, &savedMode, &dir, &savedOverride)
		if err == nil {
			if savedMode != mode || savedOverride != override || !samePath(work, w.Work) || !samePath(dir, configDir) {
				return ErrLaunchIdentityChanged
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if cc.Valid && cc.String != "" && (explicitCWD || mode != "isolated") {
			return fmt.Errorf("%w；旧会话缺少接入身份记录，不能静默迁入现有目录或用户配置", ErrLaunchIdentityChanged)
		}
		_, err = tx.Exec("INSERT INTO session_launch_identity(session,work,native_config,config_dir,config_override) VALUES(?,?,?,?,?)", session.ID, w.Work, mode, configDir, override)
		return err
	})
}
func (s *Store) StopSession(id string) error {
	_, e := s.DB.Exec("UPDATE inbox SET status='cancelled',payload=NULL WHERE session=? AND status IN ('queued','sent','consumed')", id)
	return e
}
func (s *Store) Insert(room string, m core.Message, status string) (bool, error) {
	if status == "" {
		status = "background"
	}
	j, e := json.Marshal(m)
	if e != nil {
		return false, e
	}
	r, e := s.DB.Exec("INSERT OR IGNORE INTO inbox(room,id,message,status,at) VALUES(?,?,?,?,?)", room, m.ID, string(j), status, m.Time)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n > 0, e
}
func (s *Store) Seen(room, id string) (bool, error) {
	return s.SeenContext(context.Background(), room, id)
}
func (s *Store) Background(room string, now, maxAge int64, count int) ([]core.Message, error) {
	rows, e := s.DB.Query("SELECT message FROM inbox WHERE room=? AND status='background' AND at>=? ORDER BY at DESC,id DESC LIMIT ?", room, now-maxAge, count)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []core.Message{}
	for rows.Next() {
		var raw string
		var m core.Message
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &m); e != nil {
			return nil, e
		}
		result = append(result, m)
	}
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result, rows.Err()
}
func (s *Store) ConsumeBackground(room string, ids []string) error {
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, e := tx.Exec("UPDATE inbox SET status='context' WHERE room=? AND id=? AND status='background'", room, id); e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Store) Queue(d core.Delivery) error {
	j, e := json.Marshal(d)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("UPDATE inbox SET status='queued',uuid=?,session=?,payload=? WHERE room=? AND id=?", d.UUID, d.SessionID, string(j), d.RoomKey, d.Message.ID)
	return e
}
func (s *Store) Pending(room string) ([]core.Delivery, error) {
	rows, e := s.DB.Query("SELECT payload, at FROM inbox WHERE room=? AND status='queued' ORDER BY rowid", room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []core.Delivery{}
	for rows.Next() {
		var raw string
		var at int64
		var d core.Delivery
		if e = rows.Scan(&raw, &at); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &d); e != nil {
			return nil, e
		}
		d.QueuedAt = at
		result = append(result, d)
	}
	return result, rows.Err()
}
func stringsQuery(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]string, error) {
	rows, e := q.Query(query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var v string
		if e = rows.Scan(&v); e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// PendingOutputRooms lists rooms with outputs awaiting delivery.
func (s *Store) PendingOutputRooms(ctx context.Context) ([]string, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT DISTINCT room FROM outbox WHERE state NOT IN ('done','cancelled') AND state='pending'")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if e = rows.Scan(&key); e != nil {
			return nil, e
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
func (s *Store) PendingRooms() ([]string, error) {
	return stringsQuery(s.DB, "SELECT DISTINCT room FROM inbox WHERE status='queued'")
}
func (s *Store) Mark(uuid, status string) error {
	switch status {
	case "sent", "consumed", "done", "uncertain", "cancelled":
	default:
		return errors.New("invalid inbox transition")
	}
	_, e := s.DB.Exec("UPDATE inbox SET status=? WHERE uuid=?", status, uuid)
	return e
}
func (s *Store) CountPending(room string) (int, error) {
	var n int
	e := s.DB.QueryRow("SELECT count(*) FROM inbox WHERE room=? AND status IN ('queued','sent','consumed')", room).Scan(&n)
	return n, e
}

// CountPendingChat counts pending inputs of a chat room and of every topic
// room derived from it. Topic keys extend the chat key's JSON array, so they
// sort in the half-open range [chat key without "]" + ",", ... + "-").
func (s *Store) CountPendingChat(chatKey string) (int, error) {
	return s.CountPendingChatContext(context.Background(), chatKey)
}

func (s *Store) FinishConsumed(session string) error {
	_, e := s.DB.Exec("UPDATE inbox SET status='done',payload=NULL WHERE session=? AND status='consumed'", session)
	return e
}
func (s *Store) Recover() (Recovery, error) {
	return s.RecoverWith(context.Background(), nil)
}

// RecoverWith makes recovery notices and the associated state transitions one
// transaction. beforeCommit must use tx, not Store.DB (which has one connection).
// A failed notice insertion leaves all original recovery evidence retryable.
func (s *Store) RecoverWith(ctx context.Context, beforeCommit func(*sql.Tx, Recovery) error) (Recovery, error) {
	var r Recovery
	e := s.Transaction(ctx, func(tx *sql.Tx) error {
		var e error
		r.Rooms, e = stringsQuery(tx, "SELECT DISTINCT room FROM inbox WHERE status IN ('sent','consumed')")
		if e != nil {
			return e
		}
		r.RequestRooms, e = stringsQuery(tx, "SELECT DISTINCT room FROM requests WHERE state='pending'")
		if e != nil {
			return e
		}
		_, e = tx.Exec(`UPDATE inbox SET status='uncertain' WHERE status IN ('sent','consumed'); UPDATE action_runs SET state='uncertain' WHERE state='running';
UPDATE outbox SET state=CASE WHEN kind IN ('file','artifact') AND file_state='done' AND file_target IS NOT NULL THEN 'pending' WHEN kind IN ('file','artifact') AND file_state='pending' THEN 'pending' WHEN target IS NULL OR file_state='sending' THEN 'uncertain' ELSE 'pending' END,
file_state=CASE WHEN file_state='sending' THEN 'uncertain' ELSE file_state END WHERE state IN ('sending','preparing');`)
		if e != nil {
			return e
		}
		result, e := tx.Exec("UPDATE requests SET state='expired' WHERE state='pending'")
		if e != nil {
			return e
		}
		r.Requests, e = result.RowsAffected()
		if e != nil {
			return e
		}
		if beforeCommit != nil {
			return beforeCommit(tx, r)
		}
		return nil
	})
	return r, e
}
func (s *Store) RememberContext(room string, messages []core.Message) error {
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		for _, m := range messages {
			m.Text = ""
			j, e := json.Marshal(m)
			if e != nil {
				return e
			}
			if _, e = tx.Exec("INSERT OR REPLACE INTO context_refs VALUES(?,?,?,?)", room, m.ID, string(j), time.Now().UnixMilli()); e != nil {
				return e
			}
		}
		_, e := tx.Exec("DELETE FROM context_refs WHERE room=? AND id NOT IN (SELECT id FROM context_refs WHERE room=? ORDER BY at DESC,rowid DESC LIMIT 200)", room, room)
		return e
	})
}
func (s *Store) ContextMessage(room, id string) (*core.Message, error) {
	return s.ContextMessageContext(context.Background(), room, id)
}
func (s *Store) ContextMessageContext(ctx context.Context, room, id string) (*core.Message, error) {
	var raw string
	e := s.DB.QueryRowContext(ctx, "SELECT message FROM context_refs WHERE room=? AND id=? UNION ALL SELECT message FROM inbox WHERE room=? AND id=? LIMIT 1", room, id, room, id).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var m core.Message
	if e = json.Unmarshal([]byte(raw), &m); e != nil {
		return nil, e
	}
	if m.ID == "" {
		return nil, nil
	}
	return &m, nil
}
func (s *Store) HasContextThread(room, id string) (bool, error) {
	var n int
	e := s.DB.QueryRow("SELECT 1 FROM context_refs WHERE room=? AND json_extract(message,'$.threadId')=? UNION ALL SELECT 1 FROM inbox WHERE room=? AND json_extract(message,'$.threadId')=? LIMIT 1", room, id, room, id).Scan(&n)
	if e == sql.ErrNoRows {
		return false, nil
	}
	return n == 1, e
}

// TopicThread returns the Feishu thread ID observed for the topic rooted at
// root in this room. Quoted messages from other topics never qualify.
func (s *Store) TopicThread(room, root string) (string, error) {
	var id string
	e := s.DB.QueryRow(`SELECT t FROM (
		SELECT json_extract(message,'$.threadId') AS t, json_extract(message,'$.id') AS i, json_extract(message,'$.rootId') AS r FROM context_refs WHERE room=?
		UNION ALL SELECT json_extract(message,'$.threadId'), json_extract(message,'$.id'), json_extract(message,'$.rootId') FROM inbox WHERE room=?
	) WHERE t IS NOT NULL AND t<>'' AND (i=? OR r=?) LIMIT 1`, room, room, root, root).Scan(&id)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return id, e
}

// RecentOutputs returns up to limit bot message IDs delivered in this room,
// oldest first.
func (s *Store) RecentOutputs(room string, limit int) ([]string, error) {
	rows, e := s.DB.Query("SELECT id FROM outputs WHERE room=? ORDER BY at,id LIMIT ?", room, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) DiscardInput(room, id string) error {
	_, e := s.DB.Exec("UPDATE inbox SET status='cancelled',message='{}',payload=NULL WHERE room=? AND id=?", room, id)
	return e
}
func (s *Store) RecordOutput(id, room, session string) error {
	_, e := s.DB.Exec("INSERT OR REPLACE INTO outputs VALUES(?,?,?,?)", id, room, session, time.Now().UnixMilli())
	return e
}
func (s *Store) IsOutput(room, id string) (bool, error) {
	var n int
	e := s.DB.QueryRow("SELECT 1 FROM outputs WHERE room=? AND id=?", room, id).Scan(&n)
	if e == sql.ErrNoRows {
		return false, nil
	}
	return n == 1, e
}
func (s *Store) Audit(room, event string, detail core.Args) error {
	return AuditDB(s.DB, room, event, detail)
}

// Trim keeps the legacy call shape while applying bounded, cancelable policies.
func (s *Store) Trim(now, backgroundMS int64) error {
	_, err := s.Maintain(context.Background(), now, backgroundMS)
	return err
}
