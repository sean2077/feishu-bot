package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func topicFixture(t *testing.T) *fixture {
	return newFixture(t, func(c *core.Config) { c.Groups[0].SessionScope = core.SessionScopeThread })
}
func (f *fixture) topic(root string) core.Room {
	f.t.Helper()
	r, ok := f.c.ThreadRoomFor("group", root)
	if !ok {
		f.t.Fatalf("missing topic room %s", root)
	}
	return r
}
func inTopic(m core.Message, root, thread string) core.Message {
	m.ThreadID, m.RootID, m.ReplyTo = thread, root, root
	return m
}
func (f *fixture) openSession(room string) core.Session {
	f.t.Helper()
	sessions, e := f.s.ListSessions(room)
	if e != nil {
		f.t.Fatal(e)
	}
	for _, s := range sessions {
		if s.State == "open" {
			return s
		}
	}
	f.t.Fatalf("no open session in %s", room)
	return core.Session{}
}

func TestThreadScopedGroupGivesEachTopLevelMentionItsOwnSession(t *testing.T) {
	f := topicFixture(t)
	f.send(f.message("t1", "investigate product A", "member"))
	w1 := f.worker()
	d1 := f.delivery(w1)
	f.send(f.message("t2", "fix product B", "second"))
	w2 := f.worker()
	d2 := f.delivery(w2)
	if d1.RoomKey != f.topic("t1").Key || d2.RoomKey != f.topic("t2").Key || d1.SessionID == d2.SessionID {
		t.Fatalf("top-level mentions shared a session: %+v %+v", d1, d2)
	}
	f.send(inTopic(f.message("f1", "continue", "second"), "t1", "omt_1"))
	follow := f.delivery(w1)
	if follow.SessionID != d1.SessionID || follow.RoomKey != d1.RoomKey || follow.Message.SenderID != "second" {
		t.Fatalf("topic follow-up left its session: %+v", follow)
	}
	if follow.Quote != nil {
		t.Fatal("the topic root already belongs to this session and must not be injected again")
	}
	if f.starts.Load() != 2 {
		t.Fatalf("workers started: %d", f.starts.Load())
	}
	entries, e := os.ReadDir(filepath.Join(f.c.DataDir, "rooms"))
	if e != nil || len(entries) != 1 {
		t.Fatalf("topics of one chat must share one workspace: %v %v", entries, e)
	}
	if chat, ok := RoomFor(f.c, "group", ""); !ok || chat.Key != f.topic("t1").ChatKey {
		t.Fatal("topic room lost its chat key")
	}
	if sessions, _ := f.s.ListSessions(f.room().Key); len(sessions) != 0 {
		t.Fatalf("thread scope created a chat-level session: %+v", sessions)
	}
}

func TestThreadScopedGroupIgnoresUnmentionedMessagesAndChatScopeIsUnchanged(t *testing.T) {
	f := topicFixture(t)
	top := f.message("chatter", "unrelated", "member")
	top.Mentioned = false
	f.send(top)
	inside := inTopic(f.message("aside", "unrelated", "member"), "t1", "omt_1")
	inside.Mentioned = false
	f.send(inside)
	if f.starts.Load() != 0 || f.status("chatter") != "" || f.status("aside") != "" {
		t.Fatal("unmentioned messages created topic work")
	}
	other := f.message("o1", "chat-scoped", "member")
	other.ChatID = "other"
	f.send(other)
	w := f.worker()
	d := f.delivery(w)
	f.send(inTopic(func() core.Message { m := f.message("o2", "same chat", "member"); m.ChatID = "other"; return m }(), "o1", "omt_o"))
	d2 := f.delivery(w)
	chat, _ := RoomFor(f.c, "other", "")
	if d.RoomKey != chat.Key || d2.RoomKey != chat.Key || d.SessionID != d2.SessionID {
		t.Fatalf("chat-scoped group split by topic: %+v %+v", d, d2)
	}
}

func TestTopLevelCommandsInThreadScopedGroupOnlyExplain(t *testing.T) {
	f := topicFixture(t)
	for i, cmd := range []string{"/stop", "/new", "/compact", "/status"} {
		f.send(f.message("cmd"+string(rune('a'+i)), cmd, "owner"))
	}
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		return len(f.tr.notices) == 4
	})
	f.tr.mu.Lock()
	notices := append([]string(nil), f.tr.notices...)
	f.tr.mu.Unlock()
	for _, n := range notices {
		if !strings.Contains(n, "请在对应话题内") {
			t.Fatalf("unexpected notice: %q", n)
		}
	}
	if f.starts.Load() != 0 {
		t.Fatal("a top-level command started a session")
	}
	if sessions, _ := f.s.ListSessions(f.topic("cmda").Key); len(sessions) != 0 {
		t.Fatalf("a top-level command created a topic session: %+v", sessions)
	}
}

func TestNewInsideTopicRotatesOnlyThatTopic(t *testing.T) {
	f := topicFixture(t)
	f.send(f.message("t1", "first", "member"))
	w1 := f.worker()
	f.finish(w1, f.delivery(w1), "done 1")
	f.send(f.message("t2", "second", "member"))
	w2 := f.worker()
	f.finish(w2, f.delivery(w2), "done 2")
	before1, before2 := f.openSession(f.topic("t1").Key), f.openSession(f.topic("t2").Key)
	f.send(inTopic(f.message("n1", "/new", "owner"), "t1", "omt_1"))
	eventually(t, func() bool { return f.openSession(f.topic("t1").Key).ID != before1.ID })
	if after := f.openSession(f.topic("t2").Key); after.ID != before2.ID {
		t.Fatal("/new in one topic rotated another topic")
	}
	if w2.stops.Load() != 0 {
		t.Fatal("/new in one topic stopped another topic's worker")
	}
}

func TestFirstMentionInHumanTopicCarriesRootOnce(t *testing.T) {
	f := topicFixture(t)
	root := f.message("h1", "we are discussing the release plan", "member")
	root.Mentioned = false
	f.tr.messages["h1"] = root
	f.send(inTopic(f.message("j1", "please look at this", "second"), "h1", "omt_h"))
	w := f.worker()
	d := f.delivery(w)
	if d.Quote == nil || d.Quote.ID != "h1" || d.RoomKey != f.topic("h1").Key {
		t.Fatalf("topic root was not supplied: %+v", d)
	}
	f.send(inTopic(f.message("j2", "and continue", "second"), "h1", "omt_h"))
	if d2 := f.delivery(w); d2.Quote != nil || d2.SessionID != d.SessionID {
		t.Fatalf("topic root injected repeatedly: %+v", d2)
	}
}

func TestTopicHistoryReadsOnlyItsOwnTopicWithoutTimeRange(t *testing.T) {
	f := topicFixture(t)
	f.send(f.message("t1", "first", "member"))
	w := f.worker()
	f.delivery(w)
	topic := f.topic("t1")
	session := f.openSession(topic.Key).ID
	ctx := context.Background()
	page, e := f.g.history(ctx, topic, session, core.Args{})
	if e != nil || len(page["messages"].([]core.Message)) != 0 || len(f.tr.histories) != 0 {
		t.Fatalf("topic without a Feishu thread yet: %v %v %+v", page, e, f.tr.histories)
	}
	f.send(inTopic(f.message("f1", "follow", "member"), "t1", "omt_1"))
	f.delivery(w)
	if _, e = f.g.history(ctx, topic, session, core.Args{"limit": 5}); e != nil {
		t.Fatal(e)
	}
	if len(f.tr.histories) != 1 || f.tr.histories[0].ThreadID != "omt_1" || f.tr.histories[0].StartTime != 0 || f.tr.histories[0].EndTime != 0 {
		t.Fatalf("topic history query: %+v", f.tr.histories)
	}
	for _, input := range []core.Args{{"threadId": "omt_other"}, {"startTime": 1}, {"endTime": 2}} {
		if _, e = f.g.history(ctx, topic, session, input); e == nil {
			t.Fatalf("topic history accepted %v", input)
		}
	}
	other := f.topic("t2")
	m := f.message("t2", "root", "member")
	m.ThreadID = "omt_2"
	f.tr.messages["t2"] = m
	if _, e = f.g.history(ctx, other, "session-x", core.Args{}); e != nil {
		t.Fatal(e)
	}
	if last := f.tr.histories[len(f.tr.histories)-1]; last.ThreadID != "omt_2" {
		t.Fatalf("topic thread not learned from its root: %+v", last)
	}
	if known, _ := f.s.TopicThread(other.Key, "t2"); known != "omt_2" {
		t.Fatalf("learned topic thread not remembered: %q", known)
	}
}

func TestChatHistoryThreadQueryOmitsTimeRange(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	seen := f.message("seen", "x", "member")
	seen.ThreadID = "omt_seen"
	if e := f.s.RememberContext(r.Key, []core.Message{seen}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.g.history(context.Background(), r, "s", core.Args{"threadId": "omt_seen"}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.g.history(context.Background(), r, "s", core.Args{"threadId": "omt_seen", "endTime": 5}); e == nil {
		t.Fatal("time range accepted for a thread container")
	}
	if _, e := f.g.history(context.Background(), r, "s", core.Args{}); e != nil {
		t.Fatal(e)
	}
	if len(f.tr.histories) != 2 || f.tr.histories[0].EndTime != 0 || f.tr.histories[1].EndTime == 0 {
		t.Fatalf("history queries: %+v", f.tr.histories)
	}
}

func TestCardSupplementReachesTopicTurn(t *testing.T) {
	f := topicFixture(t)
	f.send(f.message("t1", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	topic := f.topic("t1")
	card := DisplayCard("fixture", "reply", f.g.quickControls(topic, d.SessionID, d.UUID, "")...)
	if err := f.g.bindQuickControls("topic-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: topic.ChatID, UserID: "second", MessageID: "topic-card", Value: testQuickValue(t, f.g, card, "supplement"), FormValue: core.Args{"quick_supplement": "also check the logs"}}
	reply, err := f.g.Action(context.Background(), a)
	if err != nil || !strings.Contains(reply, "补充已保存") {
		t.Fatalf("supplement reply %q %v", reply, err)
	}
	extra := f.delivery(w)
	if extra.SupplementTo != d.UUID || extra.SessionID != d.SessionID || extra.RoomKey != topic.Key || extra.Message.Text != "also check the logs" {
		t.Fatalf("topic card supplement lost its turn: %+v", extra)
	}
	if status := f.status(extra.Message.ID); status == "cancelled" {
		t.Fatal("topic card supplement was cancelled after being accepted")
	}
	if reply, err = f.g.Action(context.Background(), a); err != nil || !strings.Contains(reply, "无需重复提交") {
		t.Fatalf("duplicate supplement reply %q %v", reply, err)
	}
	if _, ok := routeQueued(f.c, topic.Key, extra.Message); !ok {
		t.Fatal("card supplement was not admitted for its own topic")
	}
	f.c.Groups[0].Members = []string{"member"}
	if _, ok := routeQueued(f.c, topic.Key, extra.Message); ok {
		t.Fatal("card supplement stayed admitted after its author was removed")
	}
}

func TestNewTopicsShareTheChatRateAndInboxBudget(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Limits.MessagesPerMinute = 2
	})
	for _, id := range []string{"r1", "r2", "r3"} {
		f.send(f.message(id, "new question", "member"))
	}
	if f.status("r1") == "cancelled" || f.status("r2") == "cancelled" || f.status("r3") != "cancelled" {
		t.Fatalf("per-author chat rate bypassed by new topics: %s %s %s", f.status("r1"), f.status("r2"), f.status("r3"))
	}
	f.send(f.message("r4", "another author", "second"))
	if f.status("r4") == "cancelled" {
		t.Fatal("rate limit leaked across authors")
	}

	g := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Runtime.MaxWorkers = 1
		c.Limits.InboxPerRoom = 2
	})
	g.send(g.message("b1", "first", "member"))
	w := g.worker()
	g.delivery(w)
	g.send(g.message("b2", "second topic", "second"))
	g.send(g.message("b3", "third topic", "owner"))
	if g.status("b2") == "cancelled" || g.status("b3") != "cancelled" {
		t.Fatalf("chat inbox budget bypassed by new topics: %s %s", g.status("b2"), g.status("b3"))
	}
	if n, e := g.s.CountPendingChat(g.room().Key); e != nil || n != 2 {
		t.Fatalf("pending in chat: %d %v", n, e)
	}
	other, _ := RoomFor(g.c, "other", "")
	if n, e := g.s.CountPendingChat(other.Key); e != nil || n != 0 {
		t.Fatalf("another chat counted topic inputs: %d %v", n, e)
	}
}

func TestTopicRootIsSuppliedUntilTheSessionReceivesInput(t *testing.T) {
	f := topicFixture(t)
	root := f.message("h1", "release plan discussion", "member")
	root.Mentioned = false
	f.tr.messages["h1"] = root
	failed := make(chan struct{}, 1)
	f.tr.downloadHook = func(context.Context, string, core.Resource, string, int64) error {
		failed <- struct{}{}
		return errors.New("fixture download failure")
	}
	first := inTopic(f.message("j1", "see attachment", "second"), "h1", "omt_h")
	first.Resources = []core.Resource{{Key: "file_key", Kind: "file", Name: "a.txt"}}
	f.send(first)
	w := f.worker()
	<-failed
	eventually(t, func() bool { s := f.status("j1"); return s != "queued" && s != "" })
	if s := f.status("j1"); s == "sent" || s == "consumed" || s == "done" {
		t.Fatalf("failed preparation reached native input: %s", s)
	}
	if m, _ := f.s.ContextMessage(f.topic("h1").Key, "h1"); m == nil {
		t.Fatal("fixture precondition: the root should be cached before the failure")
	}
	f.tr.mu.Lock()
	f.tr.downloadHook = nil
	f.tr.mu.Unlock()
	f.send(inTopic(f.message("j2", "retry without attachment", "second"), "h1", "omt_h"))
	d := f.delivery(w)
	if d.Message.ID != "j2" || d.Quote == nil || d.Quote.ID != "h1" {
		t.Fatalf("root was skipped although the session never received it: %+v", d)
	}
	f.finish(w, d, "ok")
	f.send(inTopic(f.message("n1", "/new", "owner"), "h1", "omt_h"))
	old := d.SessionID
	eventually(t, func() bool { return f.openSession(f.topic("h1").Key).ID != old })
	f.send(inTopic(f.message("j3", "fresh start", "second"), "h1", "omt_h"))
	var next core.Delivery
	select {
	case next = <-w.sent:
	case nw := <-f.started:
		next = f.delivery(nw)
	case <-time.After(3 * time.Second):
		t.Fatal("input after /new not delivered")
	}
	if next.Message.ID != "j3" || next.Quote == nil || next.Quote.ID != "h1" || next.SessionID == old {
		t.Fatalf("new session in the topic did not receive the root: %+v", next)
	}
}

func TestSweepRetriesOutputsOfRoomsNotHeldInMemory(t *testing.T) {
	f := topicFixture(t)
	topic := f.topic("never-seen")
	s, e := f.s.Session(topic.Key)
	if e != nil {
		t.Fatal(e)
	}
	id, e := f.g.saveOutput(f.s.DB, topic, s.ID, "pending answer", false, "", "", "", DisplayCard("t", "pending answer"), nil, "", "", "recovery", "pending")
	if e != nil {
		t.Fatal(e)
	}
	if keys, e := f.s.PendingOutputRooms(context.Background()); e != nil || len(keys) != 1 || keys[0] != topic.Key {
		t.Fatalf("pending output rooms: %v %v", keys, e)
	}
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	eventually(t, func() bool {
		var state string
		_ = f.s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&state)
		return state == "done"
	})
	if f.tr.cardCount() == 0 || f.tr.cardLast().room.Key != topic.Key {
		t.Fatal("pending topic output was not delivered to its topic")
	}
}
