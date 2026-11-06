package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestUpgradePreservesExistingNativeSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY,room TEXT NOT NULL,cc TEXT,state TEXT NOT NULL,created INTEGER NOT NULL); INSERT INTO sessions VALUES('local-session','room','native-session','open',42); CREATE TABLE receipts(id TEXT PRIMARY KEY,room TEXT NOT NULL,session TEXT NOT NULL,desired TEXT NOT NULL,applied TEXT,at INTEGER NOT NULL); PRAGMA user_version=2;`)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	session, e := s.Session("room")
	if e != nil {
		t.Fatal(e)
	}
	if session.ID != "local-session" || session.CCID != "native-session" {
		t.Fatalf("session rotated: %+v", session)
	}
	var unknown int
	if e = s.DB.QueryRow("SELECT prior_unknown FROM usage_coverage WHERE session=?", session.ID).Scan(&unknown); e != nil || unknown != 1 {
		t.Fatal("old usage must remain incomplete", unknown, e)
	}
	var nativeTitleColumn int
	if e = s.DB.QueryRow("SELECT count(*) FROM pragma_table_info('session_preferences') WHERE name='native_title'").Scan(&nativeTitleColumn); e != nil || nativeTitleColumn != 1 {
		t.Fatalf("native title column migration missing: %d %v", nativeTitleColumn, e)
	}
	if _, e = s.Resume("other-room", session.ID); e == nil {
		t.Fatal("cross-chat resume allowed")
	}
}
func TestRestartNeverReplaysUncertainWrites(t *testing.T) {
	s := openTest(t)
	session, e := s.Session("room")
	if e != nil {
		t.Fatal(e)
	}
	m := core.Message{ID: "message", Time: time.Now().UnixMilli()}
	if _, e = s.Insert("room", m, "queued"); e != nil {
		t.Fatal(e)
	}
	d := core.Delivery{UUID: "input", RoomKey: "room", SessionID: session.ID, Message: m}
	if e = s.Queue(d); e != nil {
		t.Fatal(e)
	}
	if e = s.Mark("input", "sent"); e != nil {
		t.Fatal(e)
	}
	for _, entry := range []struct {
		id        string
		target    any
		fileState string
	}{{"unknown", nil, "none"}, {"known", "remote-message", "none"}, {"file-unknown", "remote-message", "sending"}} {
		_, e = s.DB.Exec("INSERT INTO outbox(id,room,session,text,target,success,state,at,file_state) VALUES(?,?,?,?,?,1,'sending',1,?)", entry.id, "room", session.ID, "answer", entry.target, entry.fileState)
		if e != nil {
			t.Fatal(e)
		}
	}
	r, e := s.Recover()
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Rooms) != 1 {
		t.Fatal(r)
	}
	queued, e := s.Pending("room")
	if e != nil || len(queued) != 0 {
		t.Fatal("uncertain input replayed")
	}
	for id, want := range map[string]string{"unknown": "uncertain", "known": "pending", "file-unknown": "uncertain"} {
		var got string
		if e = s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&got); e != nil || got != want {
			t.Fatal(id, got, e)
		}
	}
}
func TestQueuedPayloadIsCompatibleWithTypeScriptJSON(t *testing.T) {
	s := openTest(t)
	m := core.Message{ID: "image", Time: 1}
	if _, e := s.Insert("room", m, "queued"); e != nil {
		t.Fatal(e)
	}
	raw := `{"uuid":"uuid","roomKey":"room","sessionId":"session","message":{"id":"image","chatId":"chat","chatType":"group","senderId":"user","text":"","time":1,"mentioned":true,"resources":[],"imageOnly":true},"background":[],"attachments":[],"imageBatch":{"id":"batch","deadline":2000,"closed":false,"maxImages":8,"maxBytes":41943040}}`
	if _, e := s.DB.Exec("UPDATE inbox SET payload=?,uuid='uuid',session='session' WHERE room='room'", raw); e != nil {
		t.Fatal(e)
	}
	pending, e := s.Pending("room")
	if e != nil || len(pending) != 1 || pending[0].ImageBatch.ID != "batch" || !pending[0].Message.ImageOnly {
		t.Fatal(pending, e)
	}
	b, _ := json.Marshal(pending[0])
	var a, c map[string]any
	json.Unmarshal([]byte(raw), &a)
	json.Unmarshal(b, &c)
	if a["roomKey"] != c["roomKey"] {
		t.Fatal("JSON schema changed")
	}
}
func TestSelectedSessionRemainsVisibleAndIdentityIsStable(t *testing.T) {
	s := openTest(t)
	first, e := s.Session("room")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 25; i++ {
		if _, e = s.NewSession("room"); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Resume("room", first.ID); e != nil {
		t.Fatal(e)
	}
	list, e := s.ListSessions("room")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, session := range list {
		if session.ID == first.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("selected old session hidden")
	}
	base := t.TempDir()
	w := core.Workspace{Work: filepath.Join(base, "workspace"), Home: filepath.Join(base, "home"), NativeConfig: "isolated", ConfigDirOverride: true}
	if e = s.BindLaunchIdentity(first, w, false); e != nil {
		t.Fatal(e)
	}
	w.Work = filepath.Join(base, "other")
	if e = s.BindLaunchIdentity(first, w, false); e == nil {
		t.Fatal("silently moved existing session")
	}
}
func TestConcurrentSessionCreationKeepsOneIdentity(t *testing.T) {
	s := openTest(t)
	results := make(chan core.Session, 16)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() { session, e := s.Session("room"); results <- session; errs <- e }()
	}
	id := ""
	for i := 0; i < 16; i++ {
		r := <-results
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
		if id != "" && id != r.ID {
			t.Fatal("duplicate current sessions")
		}
		id = r.ID
	}
}
func TestFutureSchemaIsNotDowngraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	db.Exec("PRAGMA user_version=99")
	db.Close()
	if s, e := Open(path); e == nil {
		s.Close()
		t.Fatal("opened newer schema")
	}
}

func TestSQLiteFileURIPreservesWindowsDriveAndEscapesPath(t *testing.T) {
	for path, want := range map[string]string{
		"C:/service state/gateway #1.sqlite":    "/C:/service state/gateway #1.sqlite",
		"D:/state/gateway?.sqlite":              "/D:/state/gateway?.sqlite",
		"/var/lib/service state/gateway.sqlite": "/var/lib/service state/gateway.sqlite",
	} {
		u, err := url.Parse(sqliteFileURI(path))
		if err != nil || u.Host != "" || u.Scheme != "file" || u.Path != want || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("path=%q URI=%s parsed=%+v err=%v", path, sqliteFileURI(path), u, err)
		}
	}
}

func TestRecoveryNoticeFailureRollsBackEveryTransition(t *testing.T) {
	s := openTest(t)
	for _, query := range []string{
		`INSERT INTO inbox(room,id,message,status,at) VALUES('room','message','{}','sent',1)`,
		`INSERT INTO requests(id,room,session,kind,digest,state,expires) VALUES('request','room','session','permission','digest','pending',9999999999999)`,
		`INSERT INTO action_runs(id,room,session,action,digest,state,at) VALUES('action','room','session','write','digest','running',1)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('old-output','room','session','reply',1,'sending',1)`,
	} {
		if _, err := s.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("fixture notice write failed")
	_, err := s.RecoverWith(context.Background(), func(tx *sql.Tx, state Recovery) error {
		if len(state.Rooms) != 1 || len(state.RequestRooms) != 1 || state.Requests != 1 {
			t.Fatal(state)
		}
		if _, err := tx.Exec(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('notice','room','session','interrupted',0,'pending',2)`); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for query, want := range map[string]string{
		`SELECT status FROM inbox WHERE id='message'`:     "sent",
		`SELECT state FROM requests WHERE id='request'`:   "pending",
		`SELECT state FROM action_runs WHERE id='action'`: "running",
		`SELECT state FROM outbox WHERE id='old-output'`:  "sending",
	} {
		var got string
		if err := s.DB.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("recovery partially committed: got=%s want=%s err=%v", got, want, err)
		}
	}
	var notices int
	if err := s.DB.QueryRow(`SELECT count(*) FROM outbox WHERE id='notice'`).Scan(&notices); err != nil || notices != 0 {
		t.Fatal("notice did not roll back", notices, err)
	}
	_, err = s.RecoverWith(context.Background(), func(tx *sql.Tx, state Recovery) error {
		_, err := tx.Exec(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('notice','room','session','interrupted',0,'pending',2)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Recover()
	if err != nil || len(state.Rooms) != 0 || len(state.RequestRooms) != 0 || state.Requests != 0 {
		t.Fatal("successful recovery repeated evidence", state, err)
	}
}

func TestLaunchIdentityMatchesLegacyIsolatedOverrideAndRejectsProfileMigration(t *testing.T) {
	s := openTest(t)
	session, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	w := core.Workspace{Work: filepath.Join(root, "workspace"), Home: filepath.Join(root, "home")}
	if _, err := s.DB.Exec(`INSERT INTO session_launch_identity(session,work,native_config,config_dir,config_override) VALUES(?,?,'isolated',?,1)`, session.ID, w.Work, filepath.Join(w.Home, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := s.BindLaunchIdentity(session, w, false); err != nil {
		t.Fatal("legacy isolated default override was changed", err)
	}
	w.NativeConfig = "inherit"
	if err := s.BindLaunchIdentity(session, w, false); err == nil {
		t.Fatal("isolated native history migrated to inherited user profile")
	}
	second, err := s.NewSession("room")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetCC(second.ID, "existing-native-id"); err != nil {
		t.Fatal(err)
	}
	if err = s.BindLaunchIdentity(second, w, true); err == nil {
		t.Fatal("pre-identity native session adopted a new host profile")
	}
}

func TestNativeSessionIdentityCannotBeOverwrittenOrCleared(t *testing.T) {
	s := openTest(t)
	session, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"original", "original"} {
		if err = s.SetCC(session.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"replacement", ""} {
		if err = s.SetCC(session.ID, id); err == nil {
			t.Fatal("native identity was overwritten", id)
		}
	}
	current, err := s.Session("room")
	if err != nil || current.CCID != "original" {
		t.Fatal(current, err)
	}
}
