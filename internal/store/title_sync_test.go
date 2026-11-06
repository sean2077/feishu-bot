package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestTitleIntentAndAcknowledgementHaveIndependentRevisions(t *testing.T) {
	s := openTest(t)
	ss, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	automatic, err := s.PrepareTitleSync("room", ss.ID, "First message", true)
	if err != nil || automatic.State != "pending" || automatic.Confirmed != "" {
		t.Fatal(automatic, err)
	}
	if changed, err := s.FinishTitleSync("room", ss.ID, automatic.Revision, "confirmed"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if err := s.RenameSession("room", ss.ID, "Saved name"); err != nil {
		t.Fatal(err)
	}
	named, err := s.TitleSync("room", ss.ID)
	if err != nil || named.Desired != "Saved name" || named.State != "pending" || named.Confirmed != "First message" || named.Revision == automatic.Revision {
		t.Fatal("saved intent was confused with native confirmation", named, err)
	}
	if changed, err := s.FinishTitleSync("room", ss.ID, automatic.Revision, "confirmed"); err != nil || changed {
		t.Fatal("late old ACK overrode saved rename", changed, err)
	}
	if changed, err := s.FinishTitleSync("room", ss.ID, named.Revision, "rejected"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	rejected, _ := s.TitleSync("room", ss.ID)
	if rejected.State != "rejected" || rejected.Confirmed != "First message" {
		t.Fatal("rejection was reported as a successful native rename", rejected)
	}
	if err := s.RenameSession("room", ss.ID, "Latest name"); err != nil {
		t.Fatal(err)
	}
	// Startup derives from current persisted intent, never its stale first input.
	latest, err := s.PrepareTitleSync("room", ss.ID, "Different first input", true)
	if err != nil || latest.Desired != "Latest name" {
		t.Fatal(latest, err)
	}
	if changed, err := s.FinishTitleSync("room", ss.ID, named.Revision, "confirmed"); err != nil || changed {
		t.Fatal("old rejection later became success for the new revision", err)
	}
	if changed, err := s.FinishTitleSync("room", ss.ID, latest.Revision, "confirmed"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if err := s.RenameSession("room", ss.ID, "-"); err != nil {
		t.Fatal(err)
	}
	cleared, _ := s.TitleSync("room", ss.ID)
	if cleared.Desired != "First message" || cleared.Confirmed != "Latest name" || cleared.State != "pending" {
		t.Fatal("clearing manual name lost remembered automatic title", cleared)
	}
}

func TestTitleSyncPersistsUnconfirmedIntentAndRoomScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCC(ss.ID, "durable-native-session"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSession("room", ss.ID, "Saved before timeout"); err != nil {
		t.Fatal(err)
	}
	title, _ := s.TitleSync("room", ss.ID)
	if changed, err := s.FinishTitleSync("room", ss.ID, title.Revision, "unconfirmed"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	title, err = s.TitleSync("room", ss.ID)
	if err != nil || title.Desired != "Saved before timeout" || title.Confirmed != "" || title.State != "unconfirmed" {
		t.Fatal("reopen lost control outcome", title, err)
	}
	actual, _ := s.Session("room")
	if actual.ID != ss.ID || actual.CCID != "durable-native-session" {
		t.Fatal("metadata recovery rotated native identity", actual)
	}
	if _, err := s.TitleSync("other", ss.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-room title read succeeded", err)
	}
	if _, err := s.PrepareTitleSync("other", ss.ID, "intrusion", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-room title intent succeeded", err)
	}
	if changed, err := s.FinishTitleSync("other", ss.ID, title.Revision, "confirmed"); err != nil || changed {
		t.Fatal("cross-room acknowledgement succeeded", changed, err)
	}
	if _, err := s.FinishTitleSync("room", ss.ID, title.Revision, "invented"); err == nil {
		t.Fatal("unsupported outcome accepted")
	}
}
