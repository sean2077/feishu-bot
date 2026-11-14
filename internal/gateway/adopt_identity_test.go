package gateway

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
)

func cardHeaderTitle(card core.Args) string {
	title := argsOf(argsOf(card["header"])["title"])
	return stringArg(title, "content")
}

func cardBodyText(card core.Args) string {
	elements, _ := card["elements"].([]any)
	if len(elements) == 0 {
		return ""
	}
	return stringArg(argsOf(argsOf(elements[0])["text"]), "content")
}

func TestAttachRootCardShowsSessionIdentity(t *testing.T) {
	f, work, native := adoptFixture(t)
	req := attachRequest(native, work)
	req.SessionName = "example-session-82"
	req.Title = "handoff"
	result := f.g.Attach(context.Background(), req)
	if !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	if result.SessionName != "example-session-82" {
		t.Fatalf("result session name = %q", result.SessionName)
	}
	wantURL := core.TopicAppLink("oc_adopted", "omt_fixture")
	if result.TopicURL != wantURL {
		t.Fatalf("topic URL = %q, want %q", result.TopicURL, wantURL)
	}
	if len(f.tr.userCards) != 1 {
		t.Fatalf("root cards = %d", len(f.tr.userCards))
	}
	card := f.tr.userCards[0].card
	if title := cardHeaderTitle(card); !strings.Contains(title, "example-session-82 · handoff") {
		t.Fatalf("card title = %q", title)
	}
	body := cardBodyText(card)
	for _, want := range []string{work, native, "claude --resume " + native} {
		if !strings.Contains(body, want) {
			t.Fatalf("card body %q missing %q", body, want)
		}
	}
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found || record.SessionName != "example-session-82" || record.Title != "handoff" {
		t.Fatalf("record: %+v %v %v", record, found, err)
	}
}

func TestNotifyUpdatesRootTitleAndFooter(t *testing.T) {
	f, work, native := adoptFixture(t)
	firstReq := notifyRequest(native, work, "first result")
	firstReq.SessionName = "name-one"
	firstReq.Title = "task"
	first := f.g.Notify(context.Background(), firstReq)
	if !first.OK || !first.Created || first.SessionName != "name-one" || first.TopicURL == "" {
		t.Fatalf("first notify: %+v", first)
	}
	if title := cardHeaderTitle(f.tr.userCards[0].card); !strings.Contains(title, "name-one · task") {
		t.Fatalf("root title = %q", title)
	}

	secondReq := notifyRequest(native, work, "second result")
	secondReq.SessionName = "name-two"
	second := f.g.Notify(context.Background(), secondReq)
	if !second.OK || second.SessionName != "name-two" || second.Created {
		t.Fatalf("second notify: %+v", second)
	}
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found || record.SessionName != "name-two" || record.Title != "task" {
		t.Fatalf("record after rename: %+v %v %v", record, found, err)
	}
	f.tr.mu.Lock()
	updates := append([]sentCard(nil), f.tr.updates...)
	notices := append([]string(nil), f.tr.notices...)
	f.tr.mu.Unlock()
	if len(updates) != 1 || updates[0].id != first.RootMessageID {
		t.Fatalf("root card updates = %+v", updates)
	}
	if title := cardHeaderTitle(updates[0].card); !strings.Contains(title, "name-two · task") {
		t.Fatalf("updated root title = %q", title)
	}
	footer := "— name-two · " + core.ShortNative(native)
	if !strings.Contains(strings.Join(notices, "\n"), footer) {
		t.Fatalf("append footer missing: %v", notices)
	}

	// A request without a name keeps the stored name and does not rewrite the card.
	third := f.g.Notify(context.Background(), notifyRequest(native, work, "third result"))
	if !third.OK || third.SessionName != "name-two" {
		t.Fatalf("third notify: %+v", third)
	}
	f.tr.mu.Lock()
	updateCount := len(f.tr.updates)
	f.tr.mu.Unlock()
	if updateCount != 1 {
		t.Fatalf("unnamed notify rewrote the root card: %d", updateCount)
	}
}

func TestTopicStatusShowsLocalSessionIdentity(t *testing.T) {
	f, work, native := adoptFixture(t)
	req := attachRequest(native, work)
	req.SessionName = "example-session-82"
	req.HolderPID = os.Getpid()
	if started, ok := process.StartTime(req.HolderPID); ok {
		req.HolderStarted = started
	}
	result := f.g.Attach(context.Background(), req)
	if !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	room, ok := f.g.roomFromKey(record.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	text, handled, err := f.g.management("/status", "", room, "member")
	if err != nil || !handled {
		t.Fatalf("status: %v %v", handled, err)
	}
	if !strings.Contains(text, "本地会话名：example-session-82") {
		t.Fatalf("status missing local name: %s", text)
	}
	if !strings.Contains(text, "终端占用：是") || !strings.Contains(text, native) {
		t.Fatalf("status missing holder or native ID: %s", text)
	}
}

func TestTopicReplyEndsWithSessionFooter(t *testing.T) {
	f, _, native, result := relayFixture(t)
	f.g.adoptSender = func(context.Context, string, string, string) error { return nil }
	f.send(relayMessage(f, "om_footer", "hello", result.RootMessageID))
	reply := f.g.TopicReply(context.Background(), core.TopicReplyRequest{Kind: core.ControlKindReply, ID: core.NewID(), Native: native, MessageID: "om_footer", Text: "answer"})
	if !reply.OK {
		t.Fatalf("topic reply: %+v", reply)
	}
	f.tr.mu.Lock()
	notices := strings.Join(f.tr.notices, "\n")
	f.tr.mu.Unlock()
	if !strings.Contains(notices, "— fixture-session · "+core.ShortNative(native)) {
		t.Fatalf("reply footer missing: %s", notices)
	}
}

func TestAdoptionSessionNameRoundTrip(t *testing.T) {
	f, work, native := adoptFixture(t)
	if _, _, err := f.s.BeginAdoption(native, "member", work, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetAdoptionIdentity(native, "name-one", "title"); err != nil {
		t.Fatal(err)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if record.SessionName != "name-one" || record.Title != "title" {
		t.Fatalf("identity: %+v", record)
	}
	if err := f.s.SetAdoptionSessionName(native, "name-two"); err != nil {
		t.Fatal(err)
	}
	record, _, _ = f.s.AdoptedTopicByNative(native)
	if record.SessionName != "name-two" || record.State != store.AdoptedPending {
		t.Fatalf("renamed: %+v", record)
	}
}
