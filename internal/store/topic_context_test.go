package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestTopicRootConfirmationIsScopedDurableAndTransactional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	session, err := s.Session("topic-room")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s.Transaction(ctx, func(tx *sql.Tx) error {
		return s.StageTopicRoot(tx, "another-room", session.ID, "root", "input")
	}); err == nil {
		t.Fatal("another room staged context for this session")
	}
	if err = s.Transaction(ctx, func(tx *sql.Tx) error {
		return s.StageTopicRoot(tx, session.RoomKey, session.ID, "root", "input")
	}); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		got, err := s.TopicRootDelivered(ctx, session.RoomKey, session.ID, "root")
		if err != nil || got != want {
			t.Fatalf("root delivered=%v want=%v: %v", got, want, err)
		}
	}
	check(false)
	if err = s.Transaction(ctx, func(tx *sql.Tx) error {
		return s.ConfirmTopicRoot(tx, session.RoomKey, session.ID, "unrelated-input")
	}); err != nil {
		t.Fatal(err)
	}
	check(false)
	rollback := errors.New("fixture rollback")
	if err = s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := s.ConfirmTopicRoot(tx, session.RoomKey, session.ID, "input"); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	check(false)
	if err = s.Transaction(ctx, func(tx *sql.Tx) error {
		return s.ConfirmTopicRoot(tx, session.RoomKey, session.ID, "input")
	}); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	check(true)
	next, err := s.NewSession(session.RoomKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.TopicRootDelivered(ctx, next.RoomKey, next.ID, "root"); err != nil || got {
		t.Fatalf("new session inherited an older root confirmation: %v %v", got, err)
	}
}
