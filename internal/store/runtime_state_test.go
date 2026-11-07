package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestNativeLaunchFenceSurvivesReopenAndIdentityCommitIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginNativeLaunch(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BeginNativeLaunch(context.Background(), session); !errors.Is(err, ErrNativeIdentityPending) {
		t.Fatalf("reopen lost launch fence: %v", err)
	}
	if _, err = s.DB.Exec(`CREATE TRIGGER fail_identity_resolution BEFORE DELETE ON native_identity_pending BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	cc := core.NewID()
	if err = s.SetCC(session.ID, cc); err == nil {
		t.Fatal("fault did not reach the identity commit")
	}
	got, err := s.Session("room")
	if err != nil || got.CCID != "" {
		t.Fatalf("partial identity commit: %+v %v", got, err)
	}
	if _, err = s.DB.Exec("DROP TRIGGER fail_identity_resolution"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCC(session.ID, cc); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.NativeIdentityPending(session.ID); err != nil || pending {
		t.Fatalf("resolved identity left a fence: %v %v", pending, err)
	}
	if err = s.BeginNativeLaunch(context.Background(), session); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("stale empty-identity launch allowed: %v", err)
	}
	session.CCID = cc
	if err = s.BeginNativeLaunch(context.Background(), session); err != nil {
		t.Fatalf("confirmed identity could not resume: %v", err)
	}
}

func TestStopPauseAndFinalCancellationCommitTogether(t *testing.T) {
	s := openTest(t)
	session, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES('room','held','{}','queued',1,?)", session.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseRoomStop(context.Background(), "room", session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`CREATE TRIGGER fail_stop_commit BEFORE UPDATE OF status ON inbox BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishRoomStop(context.Background(), "room", session.ID); err == nil {
		t.Fatal("cancellation fault was ignored")
	}
	rooms, err := s.PausedStopRooms(context.Background())
	if err != nil || len(rooms) != 1 || rooms[0] != "room" {
		t.Fatalf("failed cancellation lost pause: %v %v", rooms, err)
	}
	if _, err = s.DB.Exec("DROP TRIGGER fail_stop_commit"); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishRoomStop(context.Background(), "room", session.ID); err != nil {
		t.Fatal(err)
	}
	rooms, err = s.PausedStopRooms(context.Background())
	var status string
	if err != nil || len(rooms) != 0 {
		t.Fatalf("completed stop kept pause: %v %v", rooms, err)
	}
	if err = s.DB.QueryRow("SELECT status FROM inbox WHERE id='held'").Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("held input not cancelled: %s %v", status, err)
	}
}

func TestUpgradeFencesLegacyLaunchWithoutARecordedIdentityOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := s.Session("legacy-room")
	untouched, _ := s.Session("untouched-room")
	work := t.TempDir()
	w := core.Workspace{Work: filepath.Join(work, "work"), Home: filepath.Join(work, "home")}
	if err = s.BindLaunchIdentity(legacy, w, false); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous schema-5 layout, which bound launch directories
	// but had no durable record of an unresolved native identity.
	if _, err = s.DB.Exec("DROP TABLE native_identity_pending"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginNativeLaunch(context.Background(), legacy); !errors.Is(err, ErrNativeIdentityPending) {
		t.Fatalf("legacy unresolved launch was treated as fresh: %v", err)
	}
	if err = s.BindLaunchIdentity(untouched, w, false); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginNativeLaunch(context.Background(), untouched); err != nil {
		t.Fatalf("upgrade blocked a never-started session: %v", err)
	}
	// No factory or Send ran, so clearing this new attempt is safe. Opening
	// the upgraded database must not repeat the legacy backfill next time.
	if err = s.ClearUnsentNativeLaunch(untouched.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BeginNativeLaunch(context.Background(), untouched); err != nil {
		t.Fatalf("upgrade backfill was repeated after a confirmed unsent launch: %v", err)
	}
	var version int
	if err = s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatalf("compatibility schema changed: %d %v", version, err)
	}
}
