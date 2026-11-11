package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func grantHistoryChat(c *core.Config, user, name string) {
	for i := range c.DMs {
		if c.DMs[i].UserID == user {
			c.DMs[i].HistoryChats = []core.HistoryChat{{ChatID: "target", Name: name}}
		}
	}
}

func crossChatFixture(t *testing.T, mutate func(*core.Config)) *fixture {
	t.Helper()
	return newFixture(t, func(c *core.Config) {
		c.Groups = append(c.Groups, core.Group{ChatID: "target", ProjectID: "project", Members: []string{"member"}, SessionScope: core.SessionScopeThread, TopicHistory: "chat"})
		c.DMs = append(c.DMs, core.DM{UserID: "stranger", ProjectID: "project"})
		grantHistoryChat(c, "owner", "Target Group")
		if mutate != nil {
			mutate(c)
		}
	})
}

func crossChatSource(t *testing.T, f *fixture, user string) (core.Room, *core.RequestContext) {
	t.Helper()
	room, ok := f.c.RoomFor("p2p_"+user, user)
	if !ok {
		t.Fatalf("missing DM room for %s", user)
	}
	return room, &core.RequestContext{ChatID: room.ChatID, ChatType: "p2p", MessageID: "m-" + user, SenderOpenID: user}
}

func crossChatAudit(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	var raw string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='history_cross_chat_read' ORDER BY seq DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func TestHistoryCrossChatReadsTargetAndAudits(t *testing.T) {
	t.Setenv("TZ", "Asia/Singapore")
	f := crossChatFixture(t, nil)
	source, requester := crossChatSource(t, f, "owner")
	now := time.Now()
	readable := core.Message{ID: "target-message", ChatID: "target", ChatType: "group", SenderID: "member", Text: "TARGET_TEXT", Time: now.Add(-time.Minute).UnixMilli(), Resources: []core.Resource{{Key: "r", Kind: "image", Name: "image.png"}}}
	stranger := core.Message{ID: "target-stranger", ChatID: "target", ChatType: "group", SenderID: "stranger", Text: "STRANGER", Time: now.UnixMilli()}
	f.tr.historyPage = core.HistoryPage{Messages: []core.Message{readable, stranger}}
	result, access, err := f.g.historyRequest(context.Background(), source, "session-a", core.Args{"chat": "Target Group", "limit": 20}, requester)
	if err != nil {
		t.Fatal(err)
	}
	if !access.CrossChat || access.Target != "target" || access.Requester != "owner" {
		t.Fatalf("access = %+v", access)
	}
	messages := result["messages"].([]core.Message)
	if len(messages) != 1 || messages[0].ID != "target-message" {
		t.Fatalf("cross-chat messages = %+v", messages)
	}
	if messages[0].Resources != nil || !messages[0].Background || messages[0].LocalTime == "" {
		t.Fatalf("cross-chat message shape = %+v", messages[0])
	}
	if result["timezone"] != "Asia/Singapore" {
		t.Fatalf("timezone = %#v", result["timezone"])
	}
	queries := f.tr.histories
	if len(queries) != 1 || queries[0].Scope != historyScopeChat || queries[0].Chat != "target" {
		t.Fatalf("cross-chat query = %+v", queries)
	}
	audit := crossChatAudit(t, f)
	if audit["target"] != "target" || audit["requester"] != "owner" || audit["count"].(float64) != 1 {
		t.Fatalf("cross-chat audit = %#v", audit)
	}
	if _, ok := audit["text"]; ok {
		t.Fatalf("cross-chat audit leaked content: %#v", audit)
	}
}

func TestHistoryCrossChatAuthorization(t *testing.T) {
	cases := []struct {
		name   string
		user   string
		mutate func(*core.Config)
		wantOK bool
	}{
		{name: "target member", user: "member", mutate: func(c *core.Config) { grantHistoryChat(c, "member", "Target Group") }, wantOK: true},
		{name: "target maintainer", user: "owner", wantOK: true},
		{name: "allowAllMembers only", user: "stranger", mutate: func(c *core.Config) {
			grantHistoryChat(c, "stranger", "Target Group")
			for i := range c.Groups {
				if c.Groups[i].ChatID == "target" {
					c.Groups[i].Members = nil
					c.Groups[i].AllowAllMembers = true
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := crossChatFixture(t, tc.mutate)
			source, requester := crossChatSource(t, f, tc.user)
			f.tr.historyPage = core.HistoryPage{Messages: []core.Message{}}
			_, _, err := f.g.historyRequest(context.Background(), source, "session-a", core.Args{"chat": "target"}, requester)
			if tc.wantOK && err != nil {
				t.Fatalf("authorized read failed: %v", err)
			}
			if !tc.wantOK && (err == nil || !strings.Contains(err.Error(), "not authorized")) {
				t.Fatalf("unauthorized read = %v", err)
			}
		})
	}
}

func TestHistoryCrossChatRejectsUnconfiguredAndInactiveSources(t *testing.T) {
	f := crossChatFixture(t, nil)
	ownerRoom, owner := crossChatSource(t, f, "owner")
	memberRoom, member := crossChatSource(t, f, "member")

	if _, _, err := f.g.historyRequest(context.Background(), memberRoom, "s", core.Args{"chat": "target"}, member); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("member DM without historyChats = %v", err)
	}
	if _, _, err := f.g.historyRequest(context.Background(), ownerRoom, "s", core.Args{"chat": "missing"}, owner); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unknown target = %v", err)
	}
	if _, _, err := f.g.historyRequest(context.Background(), ownerRoom, "s", core.Args{"chat": "target"}, nil); err == nil || !strings.Contains(err.Error(), "active DM user") {
		t.Fatalf("background read = %v", err)
	}
	group, ok := f.c.RoomFor("group", "")
	if !ok {
		t.Fatal("missing group room")
	}
	if _, _, err := f.g.historyRequest(context.Background(), group, "s", core.Args{"chat": "target"}, owner); err == nil || !strings.Contains(err.Error(), "configured DM") {
		t.Fatalf("group cross-chat read = %v", err)
	}
}

func TestHistoryCrossChatCursorBindsTarget(t *testing.T) {
	f := crossChatFixture(t, nil)
	source, requester := crossChatSource(t, f, "owner")
	f.tr.historyPage = core.HistoryPage{Messages: []core.Message{{ID: "one", ChatID: "target", ChatType: "group", SenderID: "member", Text: "one", Time: time.Now().UnixMilli()}}, NextPageToken: "next-page"}
	result, _, err := f.g.historyRequest(context.Background(), source, "session-a", core.Args{"chat": "target", "limit": 1}, requester)
	if err != nil {
		t.Fatal(err)
	}
	cursor, _ := result["nextCursor"].(string)
	if cursor == "" {
		t.Fatal("cross-chat cursor missing")
	}
	page, access, err := f.g.historyRequest(context.Background(), source, "session-a", core.Args{"cursor": cursor}, requester)
	if err != nil || !access.CrossChat || access.Target != "target" {
		t.Fatalf("cross-chat cursor page = %#v %+v %v", page, access, err)
	}
	other, otherRequester := crossChatSource(t, f, "member")
	_ = otherRequester
	if _, _, err = f.g.historyRequest(context.Background(), other, "session-a", core.Args{"cursor": cursor}, otherRequester); err == nil {
		t.Fatal("cross-chat cursor was accepted by another DM")
	}
}

func TestHistoryCrossChatExpandStripsAttachments(t *testing.T) {
	f := crossChatFixture(t, nil)
	source, requester := crossChatSource(t, f, "owner")
	now := time.Now()
	root := core.Message{ID: "root", ChatID: "target", ChatType: "group", SenderID: "member", Text: "root", ThreadID: "thread-1", RootID: "root", Time: now.Add(-time.Minute).UnixMilli(), Resources: []core.Resource{{Key: "r", Kind: "file", Name: "f.txt"}}}
	reply := core.Message{ID: "reply", ChatID: "target", ChatType: "group", SenderID: "member", Text: "reply", ThreadID: "thread-1", RootID: "root", Time: now.UnixMilli(), Resources: []core.Resource{{Key: "r2", Kind: "file", Name: "f2.txt"}}}
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			return core.HistoryPage{Messages: []core.Message{root}}, nil
		}
		return core.HistoryPage{Messages: []core.Message{root, reply}}, nil
	}
	result, access, err := f.g.historyRequest(context.Background(), source, "session-a", core.Args{"chat": "target", "expandThreads": true}, requester)
	if err != nil {
		t.Fatal(err)
	}
	if !access.CrossChat || result["expanded"] != 1 {
		t.Fatalf("cross-chat expand = %#v %+v", result, access)
	}
	for _, m := range result["messages"].([]core.Message) {
		if m.Resources != nil || m.LocalTime == "" {
			t.Fatalf("expanded cross-chat message = %+v", m)
		}
	}
}

func TestHistoryCrossChatRejectsThreadOptionsOnChatTarget(t *testing.T) {
	f := crossChatFixture(t, func(c *core.Config) {
		for i := range c.Groups {
			if c.Groups[i].ChatID == "target" {
				c.Groups[i].SessionScope = core.SessionScopeChat
				c.Groups[i].TopicHistory = "topic"
			}
		}
	})
	source, requester := crossChatSource(t, f, "owner")
	for _, input := range []core.Args{{"chat": "target", "expandThreads": true}, {"chat": "target", "threadId": "thread-1"}} {
		if _, _, err := f.g.historyRequest(context.Background(), source, "session-a", input, requester); err == nil || !strings.Contains(err.Error(), "thread-scoped target") {
			t.Fatalf("chat-scoped target accepted %v: %v", input, err)
		}
	}
}
