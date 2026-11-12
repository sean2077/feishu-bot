package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestActionRunsRequestContextColumnsMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE action_runs(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, action TEXT NOT NULL, digest TEXT NOT NULL, state TEXT NOT NULL, at INTEGER NOT NULL);
PRAGMA user_version=5;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.DB.Query("PRAGMA table_info(action_runs)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"requester", "request_context", "request", "approver", "taint_request"} {
		if !columns[name] {
			t.Fatalf("action_runs migration omitted %s: %#v", name, columns)
		}
	}
}
