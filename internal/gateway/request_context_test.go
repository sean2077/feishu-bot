package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestRequestContextUsesDeliveriesAndAnchor(t *testing.T) {
	s := &slot{
		deliveries: map[string]core.Delivery{
			"a": {UUID: "a", Message: core.Message{ID: "m1", ChatID: "oc_example", ChatType: "group", ThreadID: "om_thread", SenderID: "ou_a", SenderName: "Alice"}},
			"b": {UUID: "b", Message: core.Message{ID: "m2", ChatID: "oc_example", ChatType: "group", SenderID: "ou_b"}},
			"c": {UUID: "c", Message: core.Message{ID: "m3", ChatID: "oc_example", ChatType: "group", SenderID: "ou_a"}},
		},
		order:  []string{"a", "b", "c"},
		anchor: "a",
	}
	got := requestContextLocked(s)
	if got == nil {
		t.Fatal("missing request context")
	}
	if got.ChatID != "oc_example" || got.ChatType != "group" || got.ThreadID != "om_thread" || got.MessageID != "m1" || got.SenderOpenID != "ou_a" || got.SenderName != "Alice" {
		t.Fatalf("anchor fields = %#v", got)
	}
	if strings.Join(got.Senders, ",") != "ou_a,ou_b" {
		t.Fatalf("senders = %#v", got.Senders)
	}
	if strings.Join(got.MessageIDs, ",") != "m1,m2,m3" {
		t.Fatalf("message IDs = %#v", got.MessageIDs)
	}
	if got.RequesterLabel() != "Alice" {
		t.Fatalf("requester label = %q", got.RequesterLabel())
	}

	s.deliveries = map[string]core.Delivery{}
	s.order = nil
	s.anchor = ""
	if requestContextLocked(s) != nil {
		t.Fatal("empty deliveries produced a context")
	}

	s.deliveries = map[string]core.Delivery{"x": {UUID: "x", Message: core.Message{ID: "m4", ChatID: "oc_example", SenderID: "ou_x"}}}
	s.order = []string{"x"}
	s.anchor = "missing"
	got = requestContextLocked(s)
	if got == nil || got.MessageID != "m4" || got.SenderOpenID != "ou_x" {
		t.Fatalf("missing anchor did not fall back to the first delivery: %#v", got)
	}
}

func TestRequestContextMarksBotSenders(t *testing.T) {
	s := &slot{
		deliveries: map[string]core.Delivery{
			"human": {UUID: "human", Message: core.Message{ID: "m1", ChatID: "oc_example", ChatType: "group", SenderID: "ou_human"}},
			"bot":   {UUID: "bot", Message: core.Message{ID: "m2", ChatID: "oc_example", ChatType: "group", SenderID: "cli_other", SenderIsBot: true}},
		},
		order:  []string{"human", "bot"},
		anchor: "human",
	}
	got := requestContextLocked(s)
	if got == nil || !got.HasBotSender {
		t.Fatalf("bot sender not marked: %#v", got)
	}
	if strings.Join(got.Senders, ",") != "ou_human,cli_other" {
		t.Fatalf("senders = %#v", got.Senders)
	}
	for id := range s.deliveries {
		delivery := s.deliveries[id]
		delivery.Message.SenderIsBot = false
		s.deliveries[id] = delivery
	}
	if got = requestContextLocked(s); got == nil || got.HasBotSender {
		t.Fatalf("human-only context marked as bot: %#v", got)
	}
}

func TestGatewayBindsLiveRequestContextToWorker(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	eventually(t, func() bool { return w.requestContext() != nil })
	got := w.requestContext()
	if got == nil || got.MessageID != "one" || got.SenderOpenID != "member" || got.ChatID != "group" || got.ChatType != "group" {
		t.Fatalf("bound context = %#v", got)
	}
}

func TestActionWithoutDeliveriesHasEmptyContext(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "check", Executable: "fixture", ReadOnly: true}}
	})
	environments := make(chan map[string]string, 1)
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, env map[string]string, _ int64) (int, string, error) {
		environments <- env
		return 0, "", nil
	}
	if _, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "check", core.Args{}, core.Workspace{}, nil); err != nil {
		t.Fatal(err)
	}
	environment := <-environments
	for key := range environment {
		if strings.HasPrefix(key, "FEISHU_REQUEST_") {
			t.Fatalf("inactive action exported %s", key)
		}
	}
	var requester, stored sql.NullString
	if err := f.s.DB.QueryRow("SELECT requester, request_context FROM action_runs WHERE action='check'").Scan(&requester, &stored); err != nil {
		t.Fatal(err)
	}
	if requester.Valid || stored.Valid {
		t.Fatalf("inactive action stored context = %#v %#v", requester, stored)
	}
	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='action_started' ORDER BY seq DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal([]byte(detail), &audit); err != nil {
		t.Fatal(err)
	}
	if _, ok := audit["requester"]; ok {
		t.Fatalf("inactive action audit carried a requester: %#v", audit)
	}
	if _, ok := audit["messageId"]; ok {
		t.Fatalf("inactive action audit carried a message ID: %#v", audit)
	}
}

func TestFormattedRequesterEscapesMarkdownAndShowsOpenID(t *testing.T) {
	got := formattedRequester(&core.RequestContext{SenderName: "*Alice*\n[link](https://example.invalid) <b>", SenderOpenID: "ou_example"})
	for _, want := range []string{`\*Alice\*`, `\[link\]`, `\(https://example.invalid\)`, `\<b\>`, "（`ou_example`）"} {
		if !strings.Contains(got, want) {
			t.Fatalf("requester %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") || strings.Contains(got, "[link](https") {
		t.Fatalf("requester was not flattened or escaped: %q", got)
	}
	if got := formattedRequester(&core.RequestContext{SenderOpenID: "ou_example"}); got != "`ou_example`" {
		t.Fatalf("name-less requester = %q", got)
	}
	if got := formattedRequester(nil); got != "" {
		t.Fatalf("nil requester = %q", got)
	}
}

func TestActionCapturesContextBeforeApprovalWait(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish", Executable: "fixture"}}
	})
	environments := make(chan map[string]string, 1)
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, env map[string]string, _ int64) (int, string, error) {
		environments <- env
		return 0, "", nil
	}
	message := f.message("message-a", "work", "member")
	message.SenderName = "*Alice* [link](https://example.invalid)"
	f.send(message)
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "action-request", Kind: "action", Name: "publish", Input: core.Args{}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	cardText := jsonText(card.card)
	if !strings.Contains(cardText, `发起人：\\*Alice\\*`) || !strings.Contains(cardText, `\\[link\\]`) || !strings.Contains(cardText, "`member`") {
		t.Fatalf("approval card did not escape the requester and show the open ID: %s", cardText)
	}
	if strings.Contains(cardText, "[link](https") {
		t.Fatalf("approval card contained an unescaped link: %s", cardText)
	}

	// Change the slot while the approval is waiting; the action must keep the
	// context captured when the request arrived.
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	s.deliveries["later"] = core.Delivery{UUID: "later", Message: core.Message{ID: "message-b", ChatID: "group", ChatType: "group", SenderID: "second", SenderName: "Bob"}}
	s.order = append(s.order, "later")
	s.mu.Unlock()

	approval := cardValue(card.card, "批准一次")
	waitApprovalBinding(t, f, card, stringArg(approval, "request"))
	if _, err := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: approval}); err != nil {
		t.Fatal(err)
	}
	if got := response(t, w); !boolArg(got, "ok") {
		t.Fatalf("action failed: %#v", got)
	}
	var environment map[string]string
	select {
	case environment = <-environments:
	case <-time.After(3 * time.Second):
		t.Fatal("action did not run")
	}
	for key, want := range map[string]string{
		"FEISHU_REQUEST_CHAT_ID":        "group",
		"FEISHU_REQUEST_CHAT_TYPE":      "group",
		"FEISHU_REQUEST_MESSAGE_ID":     "message-a",
		"FEISHU_REQUEST_SENDER_OPEN_ID": "member",
		"FEISHU_REQUEST_SENDER_NAME":    "*Alice* [link](https://example.invalid)",
		"FEISHU_REQUEST_SENDERS":        "member",
	} {
		if environment[key] != want {
			t.Fatalf("%s = %q, want %q (env=%#v)", key, environment[key], want, environment)
		}
	}
	if _, ok := environment["FEISHU_REQUEST_THREAD_ID"]; ok {
		t.Fatalf("empty thread ID was exported: %#v", environment)
	}
	if strings.Contains(strings.Join([]string{environment["FEISHU_REQUEST_SENDERS"], environment["FEISHU_REQUEST_MESSAGE_ID"]}, ","), "second") {
		t.Fatal("action environment used deliveries that arrived during approval")
	}

	var requester, stored sql.NullString
	if err := f.s.DB.QueryRow("SELECT requester, request_context FROM action_runs WHERE action='publish'").Scan(&requester, &stored); err != nil {
		t.Fatal(err)
	}
	if !requester.Valid || requester.String != "member" || !stored.Valid {
		t.Fatalf("action_runs context = %#v %#v", requester, stored)
	}
	var contextValue core.RequestContext
	if err := json.Unmarshal([]byte(stored.String), &contextValue); err != nil {
		t.Fatal(err)
	}
	if contextValue.MessageID != "message-a" || strings.Join(contextValue.Senders, ",") != "member" || strings.Join(contextValue.MessageIDs, ",") != "message-a" {
		t.Fatalf("stored request context = %#v", contextValue)
	}

	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='action_started' ORDER BY seq DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal([]byte(detail), &audit); err != nil {
		t.Fatal(err)
	}
	if audit["requester"] != "member" || audit["messageId"] != "message-a" {
		t.Fatalf("action_started audit = %#v", audit)
	}
	if strings.Contains(detail, "Alice") || strings.Contains(detail, "Bob") {
		t.Fatalf("action_started audit exposed a display name: %s", detail)
	}
}
