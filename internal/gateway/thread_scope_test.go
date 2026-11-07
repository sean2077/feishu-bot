package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
