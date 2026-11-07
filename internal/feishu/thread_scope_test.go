package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func topicEvent(id, root, thread, user string) []byte {
	message := map[string]any{"message_id": id, "chat_id": "oc_allowed", "chat_type": "group", "message_type": "text", "content": `{"text":"@_user_1 hello"}`, "create_time": "1700000000123", "mentions": []any{map[string]any{"key": "@_user_1", "id": map[string]string{"open_id": "ou_bot"}, "name": "Fixture Bot"}}}
	if root != "" {
		message["root_id"] = root
		message["parent_id"] = root
	}
	if thread != "" {
		message["thread_id"] = thread
	}
	value := map[string]any{"schema": "2.0", "header": map[string]string{"event_type": "im.message.receive_v1", "event_id": "evt_" + id}, "event": map[string]any{"sender": map[string]any{"sender_id": map[string]string{"open_id": user}, "sender_type": "user"}, "message": message}}
	b, _ := json.Marshal(value)
	return b
}

type topicReply struct {
	path          string
	replyInThread any
	receiveID     any
}

func topicClient(t *testing.T, code int) (*Client, func() []topicReply) {
	t.Helper()
	var mu sync.Mutex
	var replies []topicReply
	db := fixtureDB(t)
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages/") {
			id := strings.TrimPrefix(r.URL.Path, "/open-apis/im/v1/messages/")
			apiResponse(w, map[string]any{"items": []any{fixtureItem(id, "oc_allowed", "ou_member", "text", `{"text":"quoted"}`)}})
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		body := decodedBody(t, r)
		mu.Lock()
		replies = append(replies, topicReply{r.URL.Path, body["reply_in_thread"], body["receive_id"]})
		n := len(replies)
		mu.Unlock()
		if code != 0 {
			jsonResponse(w, 400, core.Args{"code": code, "msg": "fixture refusal"})
			return
		}
		apiResponse(w, core.Args{"message_id": "om_sent_" + string(rune('a'+n))})
	})
	c.config.Groups[0].SessionScope = core.SessionScopeThread
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS inbox(room TEXT NOT NULL, id TEXT NOT NULL, message TEXT)",
		"CREATE TABLE IF NOT EXISTS context_refs(room TEXT NOT NULL, id TEXT NOT NULL, message TEXT NOT NULL, at INTEGER NOT NULL)",
		"CREATE TABLE IF NOT EXISTS outputs(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, at INTEGER NOT NULL)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return c, func() []topicReply { mu.Lock(); defer mu.Unlock(); return append([]topicReply(nil), replies...) }
}

func TestThreadScopedEventsRouteToTopicRooms(t *testing.T) {
	c, _ := topicClient(t, 0)
	var received []core.Message
	c.onMessage = func(_ context.Context, m core.Message) error { received = append(received, m); return nil }
	for _, payload := range [][]byte{
		topicEvent("om_top", "", "", "ou_member"),
		topicEvent("om_follow", "om_top", "omt_top", "ou_member"),
		topicEvent("om_quote", "om_top", "", "ou_member"),
	} {
		if _, err := c.HandleEvent(context.Background(), payload); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 3 || received[1].RootID != "om_top" || received[1].ThreadID != "omt_top" {
		t.Fatalf("received: %+v", received)
	}
	want := map[string]string{"om_top": "om_top", "om_follow": "om_top", "om_quote": "om_quote"}
	for id, root := range want {
		room, err := c.attachmentRoom(context.Background(), id)
		if err != nil || room.Thread != root {
			t.Fatalf("%s remembered in %+v (%v), want topic %s", id, room, err, root)
		}
	}
}

func TestTopicRoomRepliesStayInsideTheirTopic(t *testing.T) {
	c, replies := topicClient(t, 0)
	for _, payload := range [][]byte{topicEvent("om_top", "", "", "ou_member"), topicEvent("om_follow", "om_top", "omt_top", "ou_member"), topicEvent("om_other", "", "", "ou_member")} {
		if _, err := c.HandleEvent(context.Background(), payload); err != nil {
			t.Fatal(err)
		}
	}
	topic, ok := c.config.ThreadRoomFor("oc_allowed", "om_top")
	if !ok {
		t.Fatal("missing topic room")
	}
	other, _ := c.config.ThreadRoomFor("oc_allowed", "om_other")
	chat := fixtureRoom(t, c)
	ctx := context.Background()
	if _, err := c.Send(ctx, topic, "first answer", "om_top", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, topic, "untargeted notice", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Card(ctx, topic, core.Args{"schema": "2.0", "body": core.Args{"elements": []any{core.Args{"tag": "markdown", "content": "status"}}}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, topic, "follow-up answer", "om_follow", "omt_top"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, other, "cross-topic", "om_follow", ""); err == nil {
		t.Fatal("a topic room replied to another topic's message")
	}
	if _, err := c.Send(ctx, chat, "top-level hint", "om_other", ""); err != nil {
		t.Fatalf("chat room could not answer a top-level message of its own chat: %v", err)
	}
	got := replies()
	want := []topicReply{
		{"/open-apis/im/v1/messages/om_top/reply", true, nil},
		{"/open-apis/im/v1/messages/om_top/reply", true, nil},
		{"/open-apis/im/v1/messages/om_top/reply", true, nil},
		{"/open-apis/im/v1/messages/om_follow/reply", true, nil},
		{"/open-apis/im/v1/messages/om_other/reply", false, nil},
	}
	if len(got) != len(want) {
		t.Fatalf("replies: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reply %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTopicRootAndStoredOriginsSurviveCrossTopicQuotes(t *testing.T) {
	c, replies := topicClient(t, 0)
	ctx := context.Background()
	for _, payload := range [][]byte{topicEvent("om_top", "", "", "ou_member"), topicEvent("om_follow", "om_top", "omt_top", "ou_member")} {
		if _, err := c.HandleEvent(ctx, payload); err != nil {
			t.Fatal(err)
		}
	}
	topic, _ := c.config.ThreadRoomFor("oc_allowed", "om_top")
	other, _ := c.config.ThreadRoomFor("oc_allowed", "om_other")
	if _, err := c.db.Exec("INSERT INTO inbox(room,id,message) VALUES(?,?,'{}')", topic.Key, "om_follow"); err != nil {
		t.Fatal(err)
	}
	// Another topic quotes both messages, replacing their in-memory origin.
	for _, id := range []string{"om_top", "om_follow"} {
		if m, err := c.Fetch(ctx, id, other); err != nil || m == nil {
			t.Fatalf("quote fetch %s: %v %+v", id, err, m)
		}
	}
	if _, err := c.Send(ctx, topic, "root reply", "om_top", ""); err != nil {
		t.Fatalf("topic root reply refused: %v", err)
	}
	if _, err := c.Send(ctx, topic, "stored origin reply", "om_follow", ""); err != nil {
		t.Fatalf("stored topic origin refused: %v", err)
	}
	if len(replies()) != 2 {
		t.Fatalf("replies: %+v", replies())
	}
}

func TestUnsupportedTopicReplyIsReportedWithoutChatFallback(t *testing.T) {
	for _, code := range []int{230071, 230072} {
		c, replies := topicClient(t, code)
		if _, err := c.HandleEvent(context.Background(), topicEvent("om_top", "", "", "ou_member")); err != nil {
			t.Fatal(err)
		}
		topic, _ := c.config.ThreadRoomFor("oc_allowed", "om_top")
		_, err := c.Send(context.Background(), topic, "answer", "om_top", "")
		var failure *APIError
		if err == nil || !errors.As(err, &failure) || failure.Code != code || !strings.Contains(err.Error(), "sessionScope") {
			t.Fatalf("code %d: %v", code, err)
		}
		if got := replies(); len(got) != 1 || got[0].replyInThread != true {
			t.Fatalf("code %d fell back to another write: %+v", code, got)
		}
	}
}

func TestTopicKeyIsRejectedAfterSwitchingBackToChatScope(t *testing.T) {
	c, replies := topicClient(t, 0)
	topic, _ := c.config.ThreadRoomFor("oc_allowed", "om_top")
	c.config.Groups[0].SessionScope = core.SessionScopeChat
	if _, err := c.Send(context.Background(), topic, "stale", "", ""); err == nil || len(replies()) != 0 {
		t.Fatal("a topic room key outlived its thread scope")
	}
}
