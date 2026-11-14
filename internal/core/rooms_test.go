package core

import "testing"

func scopeConfig() Config {
	return Config{
		TenantID: "tenant",
		AppID:    "app",
		Projects: []Project{{ID: "project", Maintainers: []string{"owner"}}},
		Groups: []Group{
			{ChatID: "oc_chat", ProjectID: "project", Members: []string{"member"}},
			{ChatID: "oc_topics", ProjectID: "project", Members: []string{"member"}, SessionScope: SessionScopeThread},
		},
		DMs: []DM{{UserID: "member", ProjectID: "project"}},
	}
}

func TestChatScopedRoomKeyIsUnchanged(t *testing.T) {
	c := scopeConfig()
	r, ok := c.RoomFor("oc_chat", "")
	if !ok || r.Key != `["tenant","app","project","group","oc_chat",null]` || r.ChatKey != r.Key || r.Thread != "" || r.SessionScope != SessionScopeChat {
		t.Fatalf("chat room: %+v", r)
	}
	scoped, ok := c.RoomFor("oc_topics", "")
	if !ok || scoped.Key != `["tenant","app","project","group","oc_topics",null]` || scoped.SessionScope != SessionScopeThread {
		t.Fatalf("chat room of a thread-scoped group must keep its key: %+v", scoped)
	}
	dm, ok := c.RoomFor("oc_dm", "member")
	if !ok || dm.SessionScope != "" || dm.Key != `["tenant","app","project","p2p","oc_dm","member"]` {
		t.Fatalf("dm room: %+v", dm)
	}
}

func TestThreadRoomKeyRoundTripsOnlyWhileThreadScoped(t *testing.T) {
	c := scopeConfig()
	if _, ok := c.ThreadRoomFor("oc_chat", "om_root"); ok {
		t.Fatal("chat-scoped group produced a topic room")
	}
	r, ok := c.ThreadRoomFor("oc_topics", "om_root")
	if !ok || r.Key != `["tenant","app","project","group","oc_topics",null,"om_root"]` || r.Thread != "om_root" || r.WorkspaceKey() != `["tenant","app","project","group","oc_topics",null]` {
		t.Fatalf("topic room: %+v", r)
	}
	chat, dm, thread, ok := ParseRoomKey(r.Key)
	if !ok || chat != "oc_topics" || dm != "" || thread != "om_root" {
		t.Fatalf("parse: %q %q %q %v", chat, dm, thread, ok)
	}
	back, ok := c.RoomForKey(r.Key)
	if !ok || back.Key != r.Key || back.Thread != "om_root" {
		t.Fatalf("round trip: %+v", back)
	}
	if chatRoom, ok := c.RoomForKey(r.ChatKey); !ok || chatRoom.Thread != "" {
		t.Fatal("the chat room key of a thread-scoped group must stay valid")
	}
	c.Groups[1].SessionScope = SessionScopeChat
	if _, ok := c.RoomForKey(r.Key); ok {
		t.Fatal("topic key stayed valid after switching the group to chat scope")
	}
	for _, key := range []string{
		`["tenant","app","project","group","oc_topics","member","om_root"]`,
		`["tenant","app","project","group","oc_topics",null,"om/../x"]`,
		`["tenant","app","project","group","oc_topics",null,""]`,
		`["tenant","app","project","group","oc_topics","member"]`,
		`["tenant","app","project","group","oc_topics",null,"om_root","extra"]`,
	} {
		if _, _, _, ok := ParseRoomKey(key); ok {
			t.Fatalf("malformed key accepted: %s", key)
		}
	}
}

func TestMessageRoomUsesRealTopicsOnly(t *testing.T) {
	c := scopeConfig()
	cases := []struct {
		name string
		m    Message
		root string
	}{
		{"top-level mention roots a new topic", Message{ID: "om_top"}, "om_top"},
		{"reply inside a topic joins its root", Message{ID: "om_reply", ThreadID: "omt_1", RootID: "om_top", ReplyTo: "om_top"}, "om_top"},
		{"quoted reply without a topic starts its own", Message{ID: "om_quote", RootID: "om_top", ReplyTo: "om_top"}, "om_quote"},
		{"first post of a topic group roots itself", Message{ID: "om_first", ThreadID: "omt_2"}, "om_first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.m.ChatID, tc.m.ChatType, tc.m.SenderID = "oc_topics", "group", "member"
			r, ok := c.MessageRoom(tc.m)
			if !ok || r.Thread != tc.root {
				t.Fatalf("room %+v, want root %q", r, tc.root)
			}
			tc.m.ChatID = "oc_chat"
			r, ok = c.MessageRoom(tc.m)
			if !ok || r.Thread != "" || r.Key != r.ChatKey {
				t.Fatalf("chat-scoped group split a session: %+v", r)
			}
		})
	}
	dm, ok := c.MessageRoom(Message{ID: "om_dm", ChatID: "oc_dm", ChatType: "p2p", SenderID: "member", ThreadID: "omt_3", RootID: "om_x"})
	if !ok || dm.Thread != "" || dm.DMUser != "member" {
		t.Fatalf("dm room: %+v", dm)
	}
}

func TestWildcardBindingCarriesSessionScopeAndExplicitBindingWins(t *testing.T) {
	c := scopeConfig()
	c.Groups = append(c.Groups, Group{ChatID: AnyGroupChatID, ProjectID: "project", SessionScope: SessionScopeThread})
	if r, ok := c.ThreadRoomFor("oc_unlisted", "om_root"); !ok || r.ChatID != "oc_unlisted" {
		t.Fatalf("wildcard thread scope: %+v", r)
	}
	if _, ok := c.ThreadRoomFor("oc_chat", "om_root"); ok {
		t.Fatal("explicit chat-scoped binding inherited the wildcard thread scope")
	}
}

func TestSessionScopeChangesDoNotRotateChatRoomKey(t *testing.T) {
	c := scopeConfig()
	before, _ := c.RoomFor("oc_chat", "")
	c.Groups[0].SessionScope = SessionScopeThread
	after, _ := c.RoomFor("oc_chat", "")
	if before.Key != after.Key {
		t.Fatalf("chat room key changed with session scope: %q != %q", before.Key, after.Key)
	}
	for _, v := range []string{"", SessionScopeChat, SessionScopeThread} {
		if !ValidSessionScope(v) {
			t.Fatalf("valid scope rejected: %q", v)
		}
	}
	for _, v := range []string{"user", "Thread", "topic"} {
		if ValidSessionScope(v) {
			t.Fatalf("invalid scope accepted: %q", v)
		}
	}
}
