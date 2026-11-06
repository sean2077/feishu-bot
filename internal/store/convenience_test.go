package store

import (
	"testing"
)

func TestSessionNamesAndPrefixesStayWithinRoom(t *testing.T) {
	s := openTest(t)
	first, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSession("room", first.ID, "Release Notes"); err != nil {
		t.Fatal(err)
	}
	second, err := s.NewSession("room")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSession("room", second.ID, "release notes"); err == nil {
		t.Fatal("case-insensitive room name conflict accepted")
	}
	for _, query := range []string{first.ID, first.ID[:8], "RELEASE NOTES"} {
		id, err := s.ResolveSession("room", query)
		if err != nil || id != first.ID {
			t.Fatal("failed scoped resolution", query, id, err)
		}
		if _, err := s.ResolveSession("other-room", query); err == nil {
			t.Fatal("cross-room session resolution allowed", query)
		}
	}
	other, _ := s.Session("other-room")
	if err := s.RenameSession("other-room", other.ID, "Release Notes"); err != nil {
		t.Fatal("same name in another room should be allowed", err)
	}
	for _, id := range []string{"abcdef00-0000-4000-8000-000000000001", "abcdef00-0000-4000-8000-000000000002"} {
		if _, err := s.DB.Exec("INSERT INTO sessions(id,room,state,created) VALUES(?,'room','archived',1)", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ResolveSession("room", "abcdef00"); err == nil {
		t.Fatal("ambiguous short ID selected an arbitrary session")
	}
	if _, err := s.ResolveSession("room", "%"); err == nil {
		t.Fatal("SQL wildcard resolved a session")
	}
}

func TestNativeTitleStaysOutsideChatNameLookup(t *testing.T) {
	s := openTest(t)
	ss, err := s.Session("room")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNativeTitle("room", ss.ID, "张三：修复登录问题"); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preferences("room", ss.ID)
	if err != nil || p.NativeTitle != "张三：修复登录问题" || p.Name != "" {
		t.Fatalf("preferences %+v %v", p, err)
	}
	if _, err := s.ResolveSession("room", "张三：修复登录问题"); err == nil {
		t.Fatal("automatic native title became a chat-local session name")
	}
	if err := s.RenameSession("room", ss.ID, "-"); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Preferences("room", ss.ID)
	if p.NativeTitle != "张三：修复登录问题" || p.Name != "" {
		t.Fatalf("rename - cleared native display metadata: %+v", p)
	}
}
func TestProfileRequestRevisionDoesNotLoseNewSelection(t *testing.T) {
	s := openTest(t)
	ss, _ := s.Session("room")
	if err := s.SetCC(ss.ID, "existing-native-id"); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestProfile("room", ss.ID, "careful"); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Preferences("room", ss.ID)
	if err := s.RequestProfile("room", ss.ID, "fast"); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ApplyProfile("room", ss.ID, first.Revision); err != nil || changed {
		t.Fatal("old profile request overrode a newer selection", err)
	}
	second, _ := s.Preferences("room", ss.ID)
	if changed, err := s.ApplyProfile("room", ss.ID, second.Revision); err != nil || !changed {
		t.Fatal("latest profile did not apply", err)
	}
	p, _ := s.Preferences("room", ss.ID)
	actual, _ := s.Session("room")
	if p.Profile != "fast" || p.RequestedProfile != nil || actual.ID != ss.ID || actual.CCID != "existing-native-id" {
		t.Fatal("profile switch lost durable session identity", p, actual)
	}
	if err := s.RequestProfile("other-room", ss.ID, "careful"); err == nil {
		t.Fatal("cross-room profile write accepted")
	}
}
