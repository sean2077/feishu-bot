package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAdoptedTopicIdentityColumnsMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE adopted_topics(native TEXT PRIMARY KEY, user TEXT NOT NULL, room TEXT UNIQUE, session TEXT, thread TEXT NOT NULL DEFAULT '', root TEXT UNIQUE, work TEXT NOT NULL, holder_pid INTEGER NOT NULL DEFAULT 0, holder_started INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, created INTEGER NOT NULL, updated INTEGER NOT NULL);
INSERT INTO adopted_topics VALUES('native-old','member',NULL,NULL,'',NULL,'/work',0,0,'pending',1,1);
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
	rows, err := s.DB.Query("PRAGMA table_info(adopted_topics)")
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
	for _, name := range []string{"session_name", "title"} {
		if !columns[name] {
			t.Fatalf("adopted_topics migration omitted %s: %#v", name, columns)
		}
	}
	record, found, err := s.AdoptedTopicByNative("native-old")
	if err != nil || !found || record.SessionName != "" || record.Title != "" || record.State != AdoptedPending {
		t.Fatalf("old row: %+v %v %v", record, found, err)
	}
}
