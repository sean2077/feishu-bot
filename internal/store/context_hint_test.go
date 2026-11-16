package store

import (
	"path/filepath"
	"testing"
)

func TestContextHintMarkerPersistsPerSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := s.ContextHintSent("room", session.ID); err != nil || sent {
		t.Fatalf("fresh marker sent=%v err=%v", sent, err)
	}
	if err = s.MarkContextHintSent("room", session.ID); err != nil {
		t.Fatal(err)
	}
	if sent, err := s.ContextHintSent("room", session.ID); err != nil || !sent {
		t.Fatalf("marked marker sent=%v err=%v", sent, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if sent, err := s.ContextHintSent("room", session.ID); err != nil || !sent {
		t.Fatalf("marker did not survive restart: sent=%v err=%v", sent, err)
	}
	if err = s.ClearContextHintSent("room", session.ID); err != nil {
		t.Fatal(err)
	}
	if sent, err := s.ContextHintSent("room", session.ID); err != nil || sent {
		t.Fatalf("cleared marker sent=%v err=%v", sent, err)
	}
	if _, err = s.NewSession("room"); err != nil {
		t.Fatal(err)
	}
	if sent, err := s.ContextHintSent("room", session.ID); err != nil || sent {
		t.Fatalf("old session marker reappeared: sent=%v err=%v", sent, err)
	}
}
