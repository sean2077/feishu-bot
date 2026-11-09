package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestFeedbackTableAndReadOnlyOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES('f1','room','project','session','native','output','message','ou_example','useless','question','answer',1)`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var value string
	if err := ro.DB.QueryRow("SELECT value FROM feedback WHERE id='f1'").Scan(&value); err != nil || value != "useless" {
		t.Fatalf("read-only feedback = %q %v", value, err)
	}
	if _, err := ro.DB.Exec("INSERT INTO feedback(id) VALUES('forbidden')"); err == nil {
		t.Fatal("read-only store accepted a write")
	}
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing.sqlite")); err == nil {
		t.Fatal("read-only open accepted a missing database")
	}
}

func TestFeedbackReadOnlyWhileWriterIsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES('f1','room','project','session',NULL,'output','message','ou_example','useful','question','answer',1)`); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err := reader.DB.QueryRow("SELECT count(*) FROM feedback").Scan(&count); err != nil || count != 1 {
		t.Fatalf("reader count = %d %v", count, err)
	}
	if _, err := writer.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES('f2','room','project','session',NULL,'output2','message2','ou_example','useless','question2','answer2',2)`); err != nil {
		t.Fatal(err)
	}
	if err := reader.DB.QueryRow("SELECT count(*) FROM feedback").Scan(&count); err != nil || count != 2 {
		t.Fatalf("concurrent reader count = %d %v", count, err)
	}
}

func TestFeedbackRetentionAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UnixMilli()
	old := now - 91*24*60*60*1000
	recent := now - 89*24*60*60*1000
	for id, at := range map[string]int64{"old": old, "recent": recent} {
		if _, err := s.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES(?, 'room','project','session',NULL,?,'message','ou_example','useless','question','answer',?)`, id, id, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Maintain(context.Background(), now, 0); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM feedback WHERE id='old'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("old feedback retained: %d %v", count, err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM feedback WHERE id='recent'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("recent feedback removed: %d %v", count, err)
	}
}

func TestFeedbackTableCreatedOnExistingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA user_version=5"); err != nil {
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
	var name string
	if err := s.DB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='feedback'").Scan(&name); err != nil || name != "feedback" {
		t.Fatalf("feedback table migration = %q %v", name, err)
	}
}
