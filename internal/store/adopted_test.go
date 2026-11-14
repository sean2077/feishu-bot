package store

import (
	"path/filepath"
	"testing"
)

func adoptionFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "adopted.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err = s.NewSession("room:other"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAdoptionLifecycle(t *testing.T) {
	s := adoptionFixture(t)
	native := "11111111-2222-3333-4444-555555555555"
	a, created, err := s.BeginAdoption(native, "member", t.TempDir(), 4242, 99)
	if err != nil || !created {
		t.Fatalf("begin: %+v %v %v", a, created, err)
	}
	if a.State != AdoptedPending || a.Room != "" || a.Root != "" || a.Session != "" || a.Thread != "" || a.User != "member" {
		t.Fatalf("pending record: %+v", a)
	}
	again, created, err := s.BeginAdoption(native, "member", a.Work, 4242, 99)
	if err != nil || created || again.Native != native {
		t.Fatalf("idempotent begin: %+v %v %v", again, created, err)
	}
	claimed, err := s.NativeClaimed(native)
	if err != nil || !claimed {
		t.Fatalf("adoption not claimed: %v %v", claimed, err)
	}
	room := `["tenant","app","project","p2p","oc_dm","member","om_root"]`
	if err = s.BindAdoptionRoot(native, room, "om_root", "session-adopted", "/home/user/.claude", false, a.Created); err != nil {
		t.Fatal(err)
	}
	bound, found, err := s.AdoptedTopicByNative(native)
	if err != nil || !found || bound.Room != room || bound.Root != "om_root" || bound.Session != "session-adopted" || bound.Thread != "" {
		t.Fatalf("bound record: %+v %v %v", bound, found, err)
	}
	if err = s.OpenAdoption(native, "omt_thread"); err != nil {
		t.Fatal(err)
	}
	open, found, err := s.AdoptedTopicByThread("omt_thread")
	if err != nil || !found || open.State != AdoptedOpen || open.Native != native {
		t.Fatalf("open record: %+v %v %v", open, found, err)
	}
	if byRoot, found, err := s.AdoptedTopicByRoot("om_root"); err != nil || !found || byRoot.Room != room {
		t.Fatalf("root lookup: %+v %v %v", byRoot, found, err)
	}
	if byRoom, found, err := s.AdoptedTopicByRoom(room); err != nil || !found || byRoom.Thread != "omt_thread" {
		t.Fatalf("room lookup: %+v %v %v", byRoom, found, err)
	}
	var prior int
	if err := s.DB.QueryRow("SELECT prior_unknown FROM usage_coverage WHERE session='session-adopted'").Scan(&prior); err != nil || prior != 1 {
		t.Fatalf("adopted session must mark prior usage unknown: %d %v", prior, err)
	}
}

func TestAdoptionRejectsClaimedNative(t *testing.T) {
	s := adoptionFixture(t)
	if _, err := s.DB.Exec("INSERT INTO sessions(id,room,cc,state,created) VALUES('existing','room:existing','native-taken','open',1)"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginAdoption("native-taken", "member", t.TempDir(), 1, 0); err != ErrNativeClaimed {
		t.Fatalf("claimed native accepted: %v", err)
	}
}

func TestAdoptionStepsAreOrdered(t *testing.T) {
	s := adoptionFixture(t)
	native := "native-order"
	if _, _, err := s.BeginAdoption(native, "member", t.TempDir(), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAdoption(native, "omt_early"); err == nil {
		t.Fatal("thread opened before a root")
	}
	room := `["tenant","app","project","p2p","oc_dm","member","om_root"]`
	if err := s.BindAdoptionRoot(native, room, "om_root", "session-order", "/cfg", false, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAdoptionRoot(native, room, "om_other", "session-other", "/cfg", false, 2); err == nil {
		t.Fatal("root was bound twice")
	}
	if err := s.OpenAdoption(native, "omt_order"); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAdoption(native, "omt_again"); err == nil {
		t.Fatal("open adoption accepted another thread")
	}
}

func TestUncertainAdoptionPreservesEvidenceWithoutDowngrade(t *testing.T) {
	s := adoptionFixture(t)
	native := "native-uncertain"
	if _, _, err := s.BeginAdoption(native, "member", t.TempDir(), 7, 8); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAdoptionUncertain(native); err != nil {
		t.Fatal(err)
	}
	a, found, err := s.AdoptedTopicByNative(native)
	if err != nil || !found || a.State != AdoptedUncertain {
		t.Fatalf("uncertain record: %+v %v %v", a, found, err)
	}
	// An open topic is never downgraded by a late failure.
	other := "native-open"
	if _, _, err := s.BeginAdoption(other, "member", t.TempDir(), 0, 0); err != nil {
		t.Fatal(err)
	}
	room := `["tenant","app","project","p2p","oc_dm","member","om_root2"]`
	if err := s.BindAdoptionRoot(other, room, "om_root2", "session-open", "/cfg", true, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAdoption(other, "omt_open"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAdoptionUncertain(other); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := s.AdoptedTopicByNative(other); a.State != AdoptedOpen {
		t.Fatalf("open adoption was downgraded: %+v", a)
	}
}

func TestDeleteUnsentAdoptionOnlyBeforeRoot(t *testing.T) {
	s := adoptionFixture(t)
	native := "native-delete"
	if _, _, err := s.BeginAdoption(native, "member", t.TempDir(), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUnsentAdoption(native); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.AdoptedTopicByNative(native); found {
		t.Fatal("unsent adoption was not deleted")
	}
	kept := "native-kept"
	if _, _, err := s.BeginAdoption(kept, "member", t.TempDir(), 0, 0); err != nil {
		t.Fatal(err)
	}
	room := `["tenant","app","project","p2p","oc_dm","member","om_root3"]`
	if err := s.BindAdoptionRoot(kept, room, "om_root3", "session-kept", "/cfg", false, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUnsentAdoption(kept); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.AdoptedTopicByNative(kept); !found {
		t.Fatal("sent adoption was deleted")
	}
}

func TestAdoptionHolderClearsOnlyOnMatch(t *testing.T) {
	s := adoptionFixture(t)
	native := "native-holder"
	if _, _, err := s.BeginAdoption(native, "member", t.TempDir(), 100, 200); err != nil {
		t.Fatal(err)
	}
	if cleared, err := s.ClearAdoptionHolder(native, 100, 201); err != nil || cleared {
		t.Fatalf("mismatched holder cleared: %v %v", cleared, err)
	}
	if cleared, err := s.ClearAdoptionHolder(native, 100, 200); err != nil || !cleared {
		t.Fatalf("matching holder not cleared: %v %v", cleared, err)
	}
	a, _, err := s.AdoptedTopicByNative(native)
	if err != nil || a.HolderPID != 0 || a.HolderStarted != 0 {
		t.Fatalf("holder not cleared: %+v %v", a, err)
	}
	if err := s.UpdateAdoptionHolder(native, 300, 400); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := s.AdoptedTopicByNative(native); a.HolderPID != 300 || a.HolderStarted != 400 {
		t.Fatalf("holder not updated: %+v", a)
	}
}

func TestExpirePendingAdoptions(t *testing.T) {
	s := adoptionFixture(t)
	native := "native-expire"
	a, _, err := s.BeginAdoption(native, "member", t.TempDir(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ExpirePendingAdoptions(a.Created+1, a.Created+2); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.AdoptedTopicByNative(native); got.State != AdoptedUncertain {
		t.Fatalf("pending adoption did not expire: %+v", got)
	}
}

func TestAdoptionThreadIsUnique(t *testing.T) {
	s := adoptionFixture(t)
	first, second := "native-first", "native-second"
	for _, native := range []string{first, second} {
		if _, _, err := s.BeginAdoption(native, "member", t.TempDir(), 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BindAdoptionRoot(first, `["t","a","p","p2p","oc_dm","member","om_root_a"]`, "om_root_a", "session-first", "/cfg", false, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAdoptionRoot(second, `["t","a","p","p2p","oc_dm","member","om_root_b"]`, "om_root_b", "session-second", "/cfg", false, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAdoption(first, "omt_shared"); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAdoption(second, "omt_shared"); err == nil {
		t.Fatal("two adoption records shared one Feishu thread")
	}
}

func TestBeginAdoptionReportsNonConstraintStorageError(t *testing.T) {
	s := adoptionFixture(t)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_adoption BEFORE INSERT ON adopted_topics WHEN NEW.native='native-fail' BEGIN SELECT RAISE(FAIL,'storage probe'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginAdoption("native-fail", "member", t.TempDir(), 0, 0); err == nil || err == ErrNativeClaimed {
		t.Fatalf("storage failure was misreported: %v", err)
	}
}

func TestNativeClaimConflictCodes(t *testing.T) {
	cases := map[int]bool{
		1555: true,  // SQLITE_CONSTRAINT_PRIMARYKEY
		2067: true,  // SQLITE_CONSTRAINT_UNIQUE
		19:   false, // primary constraint code, not proof of one owner
		1811: false, // trigger RAISE(FAIL)
		0:    false,
		2068: false, // SQLITE_CONSTRAINT_CHECK
	}
	for code, want := range cases {
		if got := nativeClaimConflictCode(code); got != want {
			t.Fatalf("code %d: got %v, want %v", code, got, want)
		}
	}
}

func TestAuditDBWritesRow(t *testing.T) {
	s := adoptionFixture(t)
	if err := AuditDB(s.DB, "room", "adopted_topic_rejected", map[string]any{"reason": "root_mismatch"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM audit WHERE room='room' AND event='adopted_topic_rejected'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit row: %d %v", count, err)
	}
}
