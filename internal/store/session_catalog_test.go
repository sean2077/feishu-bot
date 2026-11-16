package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRotateIdleSessionBoundaryAndResumeRefresh(t *testing.T) {
	s := openTest(t)
	first, err := s.NewSession("room")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transaction(context.Background(), func(tx *sql.Tx) error {
		return RecordSessionActivity(tx, "room", first.ID, 1000)
	}); err != nil {
		t.Fatal(err)
	}
	const window = int64(500)
	previous, rotated, err := s.RotateIdleSession("room", window, 1500)
	if err != nil || rotated || previous != "" {
		t.Fatalf("exact threshold rotated: %q %v %v", previous, rotated, err)
	}
	previous, rotated, err = s.RotateIdleSession("room", window, 1501)
	if err != nil || !rotated || previous != first.ID {
		t.Fatalf("over threshold: %q %v %v", previous, rotated, err)
	}
	current, found, err := s.CurrentSession("room")
	if err != nil || !found || current.ID == first.ID {
		t.Fatalf("new session: %+v %v %v", current, found, err)
	}
	// Resume refreshes activity so the next input is not rotated again, even
	// when the resumed session was created long ago.
	if _, err = s.DB.Exec("UPDATE sessions SET created=1 WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE session_activity SET at=1000 WHERE session=?", first.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err = s.Resume("room", first.ID); err != nil {
		t.Fatal(err)
	}
	if previous, rotated, err = s.RotateIdleSession("room", window, now); err != nil || rotated || previous != "" {
		t.Fatalf("resume did not refresh activity: %q %v %v", previous, rotated, err)
	}
	var at int64
	if err = s.DB.QueryRow("SELECT at FROM session_activity WHERE session=?", first.ID).Scan(&at); err != nil || at < time.Now().UnixMilli()-1000 {
		t.Fatalf("resume activity = %d, %v", at, err)
	}
	if _, rotated, _ = s.RotateIdleSession("room", 0, now); rotated {
		t.Fatal("disabled window rotated")
	}
}

func TestSessionCatalogPagedSearchAndActivity(t *testing.T) {
	s := openTest(t)
	err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		for i := 0; i < 1000; i++ {
			id := fmt.Sprintf("session-%04d", i)
			if _, err := tx.Exec("INSERT INTO sessions(id,room,state,created) VALUES(?,'room','archived',?)", id, i+1); err != nil {
				return err
			}
		}
		_, err := tx.Exec("INSERT INTO sessions(id,room,state,created) VALUES('foreign','other','archived',2000)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	q := SessionQuery{Before: 2000, Limit: 20}
	for {
		items, more, err := s.SearchSessions(context.Background(), "room", q)
		if err != nil || len(items) > 20 {
			t.Fatal(items, more, err)
		}
		for _, item := range items {
			if seen[item.ID] || item.RoomKey != "room" {
				t.Fatal("duplicate or foreign session", item)
			}
			seen[item.ID] = true
		}
		if !more {
			break
		}
		last := items[len(items)-1]
		q.AfterAt, q.AfterID = last.LastInputAt, last.ID
	}
	if len(seen) != 1000 {
		t.Fatal("pagination omitted sessions", len(seen))
	}
	if err := s.SetNativeTitle("room", "session-0001", "修复 Login 100%_"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNativeTitle("room", "session-0002", "修复 Login 100%_"); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.SearchSessions(context.Background(), "room", SessionQuery{Text: "修复 LOGIN 100%_"})
	if err != nil || len(items) != 2 {
		t.Fatal("literal multi-term title search", items, err)
	}
	if items[0].DisplayName() != "修复 Login 100%_" {
		t.Fatal("native title fallback missing")
	}
	if _, err := s.ResolveSession("room", "修复 Login 100%_"); err == nil {
		t.Fatal("display title changed exact resume semantics")
	}
	if err := s.Transaction(context.Background(), func(tx *sql.Tx) error { return RecordSessionActivity(tx, "room", "session-0001", 3000) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Transaction(context.Background(), func(tx *sql.Tx) error { return RecordSessionActivity(tx, "room", "session-0001", 2500) }); err != nil {
		t.Fatal(err)
	}
	items, _, err = s.SearchSessions(context.Background(), "room", SessionQuery{})
	if err != nil || items[0].ID != "session-0001" || items[0].LastInputAt != 3000 {
		t.Fatal("activity must be monotonic", items, err)
	}
	if err := s.Transaction(context.Background(), func(tx *sql.Tx) error { return RecordSessionActivity(tx, "other", "session-0001", 5000) }); err == nil {
		t.Fatal("cross-room activity accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.SearchSessions(ctx, "room", SessionQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatal("search ignored cancellation", err)
	}
}
