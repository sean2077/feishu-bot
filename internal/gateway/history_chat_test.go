package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func topicHistoryFixture(t *testing.T, mutate func(*core.Config)) *fixture {
	t.Helper()
	return newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Groups[0].TopicHistory = "chat"
		if mutate != nil {
			mutate(c)
		}
	})
}

func historyChatSource(t *testing.T, f *fixture) core.Room {
	t.Helper()
	source := f.topic("om_root")
	err := f.s.RememberContext(source.Key, []core.Message{{
		ID: "om_root", ChatID: "group", ChatType: "group", SenderID: "member",
		ThreadID: "omt_current", RootID: "om_root", Time: time.Now().Add(-time.Hour).UnixMilli(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func historyTestMessage(id, sender, text string, at time.Time, thread, root string) core.Message {
	return core.Message{ID: id, ChatID: "group", ChatType: "group", SenderID: sender, Text: text, Time: at.UnixMilli(), ThreadID: thread, RootID: root, Resources: []core.Resource{}}
}

func historyChatAuditDetail(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	var raw string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='history_chat_scope_read' ORDER BY seq DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func TestHistoryChatScopeRequiresExplicitConfiguration(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Groups[0].SessionScope = core.SessionScopeThread })
	source := f.topic("om_root")
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat"}); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("unconfigured chat scope accepted: %v", err)
	}

	f2 := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeChat
		c.Groups[0].TopicHistory = "chat"
	})
	f2.tr.historyPage = core.HistoryPage{Messages: []core.Message{}}
	if _, err := f2.g.history(context.Background(), f2.room(), "session-a", core.Args{"scope": "chat"}); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("chat-scoped group accepted chat history: %v", err)
	}
}

func TestHistoryChatScopeRejectsDMsWithFallbackGroup(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups = append(c.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "project", SessionScope: core.SessionScopeThread, TopicHistory: "chat"})
	})
	var calls atomic.Int64
	f.tr.historyHook = func(context.Context, core.Room, core.HistoryQuery) (core.HistoryPage, error) {
		calls.Add(1)
		return core.HistoryPage{}, nil
	}
	source, ok := f.c.RoomFor("oc_p2p_example", "member")
	if !ok || source.DMUser != "member" {
		t.Fatalf("dm room = %+v", source)
	}
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat"}); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("dm chat scope accepted: %v", err)
	}
	cursor := f.g.sealHistoryCursor(source.Key, "session-a", core.HistoryQuery{Limit: 20, Scope: historyScopeChat, Chat: source.ChatID})
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"cursor": cursor}); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("dm chat cursor accepted: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("dm chat scope reached transport %d times", calls.Load())
	}
}

func TestHistoryChatScopeTimelineBackgroundAndBackfill(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	otherTopic := f.topic("om_other_root")
	historyOutputRow(t, f, "out-card", otherTopic.Key, "om_card", "backfilled answer", "reply", "done")
	otherChat, _ := RoomFor(f.c, "other", "")
	historyOutputRow(t, f, "out-other-chat", otherChat.Key, "om_card", "wrong chat", "reply", "done")

	other := historyTestMessage("om_other_root", "member", "OTHER_TOPIC_TEXT", now.Add(-2*time.Hour), "omt_other", "om_other_root")
	top := historyTestMessage("om_top", "member", "TOP_LEVEL_TEXT", now.Add(-time.Hour), "", "")
	current := historyTestMessage("om_current", "member", "CURRENT_TOPIC_TEXT", now.Add(-30*time.Minute), "omt_current", "om_root")
	stranger := historyTestMessage("om_stranger", "stranger", "STRANGER_TEXT", now.Add(-20*time.Minute), "omt_other", "om_other_root")
	card := historyTestMessage("om_card", "bot", core.HistoryCardPlaceholder, now.Add(-10*time.Minute), "omt_other", "om_other_root")
	card.SenderIsBot, card.SenderIsSelf = true, true
	start := now.Add(-3 * time.Hour).Unix()
	end := now.Unix()
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID != "" {
			t.Fatalf("unexpected thread query: %+v", q)
		}
		if q.Scope != historyScopeChat || q.Chat != "group" || q.StartTime != start || q.EndTime != end {
			t.Fatalf("chat query = %+v, want times %d..%d", q, start, end)
		}
		return core.HistoryPage{Messages: []core.Message{other, top, current, stranger, card}, NextPageToken: "next-page"}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "startTime": start, "endTime": end, "limit": 20})
	if err != nil {
		t.Fatal(err)
	}
	messages := page["messages"].([]core.Message)
	byID := map[string]core.Message{}
	for _, m := range messages {
		byID[m.ID] = m
	}
	if len(byID) != 4 || byID["om_stranger"].ID != "" {
		t.Fatalf("chat scope readable set = %#v", byID)
	}
	if !byID["om_other_root"].Background || !byID["om_top"].Background || !byID["om_card"].Background || byID["om_current"].Background {
		t.Fatalf("background flags = %+v", byID)
	}
	if byID["om_card"].Text != "backfilled answer" {
		t.Fatalf("same-chat backfill = %q", byID["om_card"].Text)
	}
	if page["nextCursor"] == nil || page["hasMore"] != true {
		t.Fatalf("chat cursor = %#v", page)
	}
	audit := historyChatAuditDetail(t, f)
	threads, _ := audit["threads"].([]any)
	if audit["room"] != source.Key || audit["session"] != "session-a" || audit["count"].(float64) != 4 || len(threads) != 2 {
		t.Fatalf("chat audit = %#v", audit)
	}
	raw, _ := json.Marshal(audit)
	for _, marker := range []string{"OTHER_TOPIC_TEXT", "TOP_LEVEL_TEXT", "CURRENT_TOPIC_TEXT", "backfilled answer"} {
		if strings.Contains(string(raw), marker) {
			t.Fatalf("audit leaked content: %s", raw)
		}
	}

	cursor, _ := page["nextCursor"].(string)
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.PageToken != "next-page" || q.Scope != historyScopeChat || q.Chat != "group" || q.ExpandThreads || q.ThreadID != "" {
			t.Fatalf("cursor query changed scope: %+v", q)
		}
		return core.HistoryPage{Messages: []core.Message{top}}, nil
	}
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"cursor": cursor}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"cursor": cursor, "scope": "chat"}); err == nil {
		t.Fatal("cursor accepted a scope switch")
	}
}

func TestHistoryChatScopeThreadMustBeObservedAndBackground(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "threadId": "omt_missing"}); err == nil || !strings.Contains(err.Error(), "observed") {
		t.Fatalf("unobserved thread accepted: %v", err)
	}
	if err := f.s.RememberContext(source.Key, []core.Message{{ID: "om_seen", ChatID: "group", ChatType: "group", SenderID: "member", ThreadID: "omt_other", RootID: "om_other", Time: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	reply := historyTestMessage("om_reply", "member", "THREAD_REPLY", time.Now(), "omt_other", "om_other")
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID != "omt_other" || q.StartTime > 0 || q.EndTime > 0 {
			t.Fatalf("thread query = %+v", q)
		}
		return core.HistoryPage{Messages: []core.Message{reply}}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "threadId": "omt_other"})
	if err != nil {
		t.Fatal(err)
	}
	messages := page["messages"].([]core.Message)
	if len(messages) != 1 || !messages[0].Background || messages[0].Text != "THREAD_REPLY" {
		t.Fatalf("thread messages = %+v", messages)
	}
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "threadId": "omt_other", "startTime": 1, "endTime": 2}); err == nil {
		t.Fatal("thread history accepted a time range")
	}
	audit := historyChatAuditDetail(t, f)
	threads, _ := audit["threads"].([]any)
	if len(threads) != 1 || threads[0] != "omt_other" || audit["count"].(float64) != 1 {
		t.Fatalf("thread audit = %#v", audit)
	}
}

func TestHistoryChatScopeExpandsRecentTopicsAndOmitsOldest(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	roots := make([]core.Message, 21)
	replies := map[string]core.Message{}
	for i := range roots {
		thread := "omt_" + string(rune('a'+i))
		rootID := "om_root_" + string(rune('a'+i))
		roots[i] = historyTestMessage(rootID, "member", "ROOT_"+string(rune('a'+i)), now.Add(-time.Duration(21-i)*time.Minute), thread, rootID)
		replies[thread] = historyTestMessage("om_reply_"+string(rune('a'+i)), "member", "REPLY_"+string(rune('a'+i)), now.Add(-time.Duration(20-i)*time.Minute), thread, rootID)
	}
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			return core.HistoryPage{Messages: roots}, nil
		}
		if reply, ok := replies[q.ThreadID]; ok {
			return core.HistoryPage{Messages: []core.Message{reply}}, nil
		}
		return core.HistoryPage{}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "expandThreads": true, "startTime": now.Add(-time.Hour).Unix(), "endTime": now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if page["threads"].(int) != 21 || page["expanded"].(int) != 20 || page["omittedThreads"].(int) != 1 || page["omittedReplies"].(int) != 1 {
		t.Fatalf("expand stats = %#v", page)
	}
	messages := page["messages"].([]core.Message)
	seen := map[string]bool{}
	for _, m := range messages {
		seen[m.ID] = true
	}
	if seen["om_root_a"] || !seen["om_root_u"] {
		t.Fatalf("oldest/newest topic selection wrong: %#v", seen)
	}
}

func TestHistoryChatScopeExpansionReducesRepliesBeforeTopics(t *testing.T) {
	f := topicHistoryFixture(t, func(c *core.Config) { c.Limits.TextChars = 100 })
	source := historyChatSource(t, f)
	now := time.Now()
	rootA := historyTestMessage("om_root_a", "member", strings.Repeat("a", 40), now.Add(-time.Hour), "omt_a", "om_root_a")
	rootB := historyTestMessage("om_root_b", "member", strings.Repeat("b", 40), now.Add(-2*time.Hour), "omt_b", "om_root_b")
	replies := map[string][]core.Message{
		"omt_a": {
			historyTestMessage("om_a1", "member", strings.Repeat("1", 100), now.Add(-30*time.Minute), "omt_a", "om_root_a"),
			historyTestMessage("om_a2", "member", strings.Repeat("2", 100), now.Add(-40*time.Minute), "omt_a", "om_root_a"),
		},
		"omt_b": {
			historyTestMessage("om_b1", "member", strings.Repeat("3", 100), now.Add(-50*time.Minute), "omt_b", "om_root_b"),
			historyTestMessage("om_b2", "member", strings.Repeat("4", 100), now.Add(-60*time.Minute), "omt_b", "om_root_b"),
		},
	}
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			return core.HistoryPage{Messages: []core.Message{rootA, rootB}}, nil
		}
		return core.HistoryPage{Messages: replies[q.ThreadID]}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "expandThreads": true, "startTime": now.Add(-3 * time.Hour).Unix(), "endTime": now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if page["threads"].(int) != 2 || page["expanded"].(int) != 2 || page["omittedThreads"].(int) != 0 || page["omittedReplies"].(int) != 4 {
		t.Fatalf("reply budget stats = %#v", page)
	}
	messages := page["messages"].([]core.Message)
	if len(messages) != 2 || messages[0].ID[0] != 'o' || messages[1].ID[0] != 'o' {
		t.Fatalf("reply budget did not retain both roots: %+v", messages)
	}
}

func TestHistoryChatScopeExpansionReducesOldestTopicsAfterReplies(t *testing.T) {
	f := topicHistoryFixture(t, func(c *core.Config) { c.Limits.TextChars = 100 })
	source := historyChatSource(t, f)
	now := time.Now()
	roots := []core.Message{
		historyTestMessage("om_root_a", "member", strings.Repeat("a", 100), now.Add(-time.Hour), "omt_a", "om_root_a"),
		historyTestMessage("om_root_b", "member", strings.Repeat("b", 100), now.Add(-2*time.Hour), "omt_b", "om_root_b"),
		historyTestMessage("om_root_c", "member", strings.Repeat("c", 100), now.Add(-3*time.Hour), "omt_c", "om_root_c"),
	}
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			return core.HistoryPage{Messages: roots}, nil
		}
		return core.HistoryPage{}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "expandThreads": true, "startTime": now.Add(-4 * time.Hour).Unix(), "endTime": now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if page["threads"].(int) != 3 || page["expanded"].(int) != 2 || page["omittedThreads"].(int) != 1 || page["omittedReplies"].(int) != 0 {
		t.Fatalf("topic budget stats = %#v", page)
	}
	messages := page["messages"].([]core.Message)
	for _, m := range messages {
		if m.ID == "om_root_c" {
			t.Fatal("oldest topic was not omitted after reply reduction")
		}
	}
}

func TestHistoryChatScopeExpansionFetchesThreadsConcurrently(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	roots := []core.Message{
		historyTestMessage("om_root_a", "member", "A", now.Add(-time.Hour), "omt_a", "om_root_a"),
		historyTestMessage("om_root_b", "member", "B", now.Add(-2*time.Hour), "omt_b", "om_root_b"),
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	f.tr.historyHook = func(ctx context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			return core.HistoryPage{Messages: roots}, nil
		}
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return core.HistoryPage{Messages: []core.Message{historyTestMessage("om_reply_"+q.ThreadID, "member", "reply", now, q.ThreadID, "root")}}, nil
		case <-ctx.Done():
			return core.HistoryPage{}, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "expandThreads": true, "startTime": now.Add(-3 * time.Hour).Unix(), "endTime": now.Unix()})
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("thread fetches were serial")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("thread fetch count = %d", calls.Load())
	}
}

func TestHistoryChatScopeBackfillDoesNotCrossChat(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	otherTopic := f.topic("om_other_root")
	otherChat, _ := RoomFor(f.c, "other", "")
	historyOutputRow(t, f, "out-same", otherTopic.Key, "om_card", "same chat", "reply", "done")
	historyOutputRow(t, f, "out-other", otherChat.Key, "om_card", "other chat", "reply", "done")
	wrongApp, _ := json.Marshal([]any{f.c.TenantID, "other-app", "project", "group", "group", nil})
	wrongKind, _ := json.Marshal([]any{f.c.TenantID, f.c.AppID, "project", "p2p", "group", "member"})
	historyOutputRow(t, f, "out-wrong-app", string(wrongApp), "om_card", "wrong app", "reply", "done")
	historyOutputRow(t, f, "out-wrong-kind", string(wrongKind), "om_card", "wrong kind", "reply", "done")
	card := historyTestMessage("om_card", "bot", core.HistoryCardPlaceholder, now, "omt_other", "om_other_root")
	card.SenderIsBot, card.SenderIsSelf = true, true
	f.tr.historyHook = func(_ context.Context, _ core.Room, _ core.HistoryQuery) (core.HistoryPage, error) {
		return core.HistoryPage{Messages: []core.Message{card}}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat"})
	if err != nil {
		t.Fatal(err)
	}
	messages := page["messages"].([]core.Message)
	if len(messages) != 1 || messages[0].Text != "same chat" {
		t.Fatalf("chat scope backfill = %+v", messages)
	}
}

func TestHistoryChatScopeExpandRejectsCombinations(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	for _, input := range []core.Args{
		{"scope": "topic", "expandThreads": true},
		{"scope": "chat", "expandThreads": true, "threadId": "omt_current"},
		{"scope": "wat"},
	} {
		if _, err := f.g.history(context.Background(), source, "session-a", input); err == nil {
			t.Fatalf("invalid history combination accepted: %#v", input)
		}
	}
	if f.tr.histories != nil {
		t.Fatalf("invalid history reached the transport: %+v", f.tr.histories)
	}
}

func TestHistoryChatScopeCursorCarriesScopeAndRejectsSwitch(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	top := historyTestMessage("om_top", "member", "TOP", now, "", "")
	f.tr.historyHook = func(_ context.Context, _ core.Room, _ core.HistoryQuery) (core.HistoryPage, error) {
		return core.HistoryPage{Messages: []core.Message{top}, NextPageToken: "next"}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat"})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := page["nextCursor"].(string)
	if token == "" {
		t.Fatal("missing chat cursor")
	}
	query, err := f.g.openHistoryCursor(token, source.Key, "session-a")
	if err != nil || query.Scope != historyScopeChat || query.Chat != "group" {
		t.Fatalf("chat cursor scope = %+v, %v", query, err)
	}
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"cursor": token, "scope": "topic"}); err == nil {
		t.Fatal("cursor allowed a scope switch")
	}
}

func TestHistoryChatScopeExpandStopsOnThreadFailure(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	root := historyTestMessage("om_root_a", "member", "A", now.Add(-time.Hour), "omt_a", "om_root_a")
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			return core.HistoryPage{Messages: []core.Message{root}}, nil
		}
		return core.HistoryPage{}, context.DeadlineExceeded
	}
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "expandThreads": true}); err == nil {
		t.Fatal("thread failure was hidden")
	}
}

func TestHistoryChatScopeExpandWidensDiscoveryForRecentReplies(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	end := now.Unix()
	root := historyTestMessage("om_old_root", "member", "OLD_ROOT", now.Add(-48*time.Hour), "omt_old", "om_old_root")
	reply := historyTestMessage("om_new_reply", "member", "NEW_REPLY", now.Add(-30*time.Minute), "omt_old", "om_old_root")
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			if q.StartTime != end-3*86400 || q.EndTime != end {
				t.Fatalf("discovery window = %+v, want widened 3 days", q)
			}
			return core.HistoryPage{Messages: []core.Message{root}}, nil
		}
		return core.HistoryPage{Messages: []core.Message{reply}}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "expandThreads": true, "startTime": time.Now().Add(-time.Hour).Unix(), "endTime": end})
	if err != nil {
		t.Fatal(err)
	}
	if page["threads"].(int) != 1 || page["expanded"].(int) != 1 || page["omittedThreads"].(int) != 0 {
		t.Fatalf("recent reply was not discovered from an older root: %#v", page)
	}
	messages := page["messages"].([]core.Message)
	foundReply := false
	for _, m := range messages {
		if m.ID == "om_new_reply" {
			foundReply = true
		}
	}
	if !foundReply {
		t.Fatal("recent reply missing from expanded history")
	}
}

func TestHistoryChatScopeRejectsThreadFromAnotherGroup(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	if err := f.s.RememberContext(source.Key, []core.Message{{ID: "om_foreign", ChatID: "other", ChatType: "group", SenderID: "member", ThreadID: "omt_foreign", RootID: "om_foreign", Time: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	foreign := historyTestMessage("om_foreign_reply", "member", "FOREIGN", time.Now(), "omt_foreign", "om_foreign")
	foreign.ChatID = "other"
	f.tr.historyHook = func(_ context.Context, _ core.Room, _ core.HistoryQuery) (core.HistoryPage, error) {
		return core.HistoryPage{Messages: []core.Message{foreign}}, nil
	}
	if _, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "threadId": "omt_foreign"}); err == nil || !strings.Contains(err.Error(), "belong") {
		t.Fatalf("foreign group thread accepted: %v", err)
	}
}

func TestHistoryChatScopeExpandSkipsFailedThreads(t *testing.T) {
	f := topicHistoryFixture(t, nil)
	source := historyChatSource(t, f)
	now := time.Now()
	first := historyTestMessage("om_root_a", "member", "A", now.Add(-time.Hour), "omt_a", "om_root_a")
	second := historyTestMessage("om_root_b", "member", "B", now.Add(-2*time.Hour), "omt_b", "om_root_b")
	f.tr.historyHook = func(_ context.Context, _ core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
		if q.ThreadID == "" {
			return core.HistoryPage{Messages: []core.Message{first, second}}, nil
		}
		if q.ThreadID == "omt_a" {
			return core.HistoryPage{}, context.DeadlineExceeded
		}
		return core.HistoryPage{Messages: []core.Message{historyTestMessage("om_reply_b", "member", "B_REPLY", now, "omt_b", "om_root_b")}}, nil
	}
	page, err := f.g.history(context.Background(), source, "session-a", core.Args{"scope": "chat", "expandThreads": true, "startTime": now.Add(-3 * time.Hour).Unix(), "endTime": now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if page["threads"].(int) != 2 || page["expanded"].(int) != 1 || page["omittedThreads"].(int) != 1 || page["failedThreads"].(int) != 1 {
		t.Fatalf("failed thread stats = %#v", page)
	}
	messages := page["messages"].([]core.Message)
	if len(messages) != 2 {
		t.Fatalf("failed thread did not degrade atomically: %+v", messages)
	}
	for _, message := range messages {
		if message.ThreadID != "omt_b" {
			t.Fatalf("failed thread leaked into results: %+v", messages)
		}
	}
}
