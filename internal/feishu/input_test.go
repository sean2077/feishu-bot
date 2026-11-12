package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestRawEventJourneyAuthorChatAndMentionGate(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("rejected input performed HTTP: %s", r.URL.Path)
	})
	var received []core.Message
	c.onMessage = func(_ context.Context, m core.Message) error { received = append(received, m); return nil }
	for _, payload := range [][]byte{
		rawEvent("oc_forbidden", "ou_member", "text", `{"text":"@_user_1 hello"}`, true),
		rawEvent("oc_allowed", "ou_stranger", "merge_forward", `{"message_ids":["om_other"]}`, true),
		rawEvent("oc_allowed", "ou_member", "text", `{"text":"@Fixture Bot hello"}`, false),
		rawEvent("oc_allowed", "ou_member", "text", `{"text":"@_all hello"}`, false),
	} {
		if _, err := c.HandleEvent(context.Background(), payload); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 0 {
		t.Fatalf("rejected messages reached gateway: %d", len(received))
	}
	payload := rawEvent("oc_allowed", "ou_member", "text", `{"text":"@_user_1 analyse @Fixture Bot literally"}`, true)
	if _, err := c.HandleEvent(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].Text != "analyse @Fixture Bot literally" || !received[0].Mentioned {
		t.Fatalf("normalization: %+v", received)
	}
	if received[0].ReplyTo != "om_parent" || received[0].ThreadID != "omt_thread" || received[0].Time != 1700000000123 {
		t.Fatal("lost origin/thread/time")
	}
	encoded, _ := json.Marshal(received[0])
	if strings.Contains(string(encoded), "raw") || strings.Contains(string(encoded), "event_id") {
		t.Fatal("raw provider payload retained")
	}
	if _, err := c.HandleEvent(context.Background(), rawEvent("oc_allowed", "ou_owner", "merge_forward", `{"message_ids":["om_other"]}`, true)); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 || received[1].InputIssue != forwardedInput || len(received[1].Resources) != 0 {
		t.Fatal("merged forward should be refused without expansion")
	}
}
func TestPostContentV2LocalesMediaAndFiles(t *testing.T) {
	payload := rawEvent("oc_allowed", "ou_member", "post", `{"zh_cn":{"title":"报告","content":[[{"tag":"text","text":"obsolete"}]],"content_v2":[[{"tag":"at","user_id":"ou_bot","user_name":"Fixture Bot"},{"tag":"text","text":"请分析"},{"tag":"img","image_key":"img_one"},{"tag":"media","file_key":"file_video","file_name":"clip.mp4"}],[{"tag":"a","text":"来源","href":"https://example.com"},{"tag":"code_block","language":"go","text":"x := 1"}]]},"en_us":{"title":"DO NOT DUPLICATE"},"files":[{"file_key":"file_report","file_name":"report.csv"}]}`, true)
	var envelope struct {
		Event json.RawMessage `json:"event"`
	}
	_ = json.Unmarshal(payload, &envelope)
	m, err := NormalizeEvent(envelope.Event, "ou_bot")
	if err != nil {
		t.Fatal(err)
	}
	if m.InputIssue != "" || strings.Contains(m.Text, "obsolete") || strings.Contains(m.Text, "DO NOT") || strings.Contains(m.Text, "Fixture Bot") {
		t.Fatalf("rendered incorrect locale/body: %+v", m)
	}
	if !strings.Contains(m.Text, "请分析") || !strings.Contains(m.Text, "```go") || !strings.Contains(m.Text, "https://example.com") {
		t.Fatal("rich content dropped")
	}
	if len(m.Resources) != 3 || m.Resources[0].Key != "img_one" || m.Resources[1].Key != "file_video" || m.Resources[2].Key != "file_report" {
		t.Fatalf("resources: %+v", m.Resources)
	}
	if m.ImageOnly {
		t.Fatal("rich post must not enter standalone image batching")
	}
}
func TestPostMentionKeyIsStrippedForCommands(t *testing.T) {
	payload := rawEvent("oc_allowed", "ou_member", "post", `{"zh_cn":{"title":"","content":[[{"tag":"at","user_id":"@_user_1","user_name":"Fixture Bot","style":[]},{"tag":"text","text":" /status --json","style":[]}]]}}`, true)
	var envelope struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	m, err := NormalizeEvent(envelope.Event, "ou_bot")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Mentioned || m.Text != "/status --json" {
		t.Fatalf("post mention key was not stripped: %+v", m)
	}
}

func TestIncompleteContentNeverKeepsPartialResources(t *testing.T) {
	cases := []struct{ kind, content, issue string }{
		{"text", `{"text":42}`, invalidInput},
		{"image", `{"image_key":""}`, invalidInput},
		{"file", `{"file_key":"file_one","file_name":23}`, invalidInput},
		{"post", `{"content":[[{"tag":"img","image_key":"img_one"},{"tag":"table","text":"lost data"}]]}`, unsupportedInput},
		{"post", `{"content":[[{"tag":"text","text":"partial"}]],"files":[{"file_key":"file_folder","is_folder":true}]}`, folderInput},
		{"post", `{"content":[[{"tag":"img","image_key":"img_one"},{"tag":"media"}]]}`, invalidInput},
		{"merge_forward", `{}`, forwardedInput},
		{"share_chat", `{}`, unsupportedInput},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			e := messageEvent{}
			e.Message.Type = tc.kind
			e.Message.Content = tc.content
			m := normalizeMessage(e, "ou_bot", true)
			if m.InputIssue != tc.issue || len(m.Resources) != 0 || !strings.Contains(m.Text, tc.issue) {
				t.Fatalf("partial content passed: %+v", m)
			}
		})
	}
}
func TestCardActionRawPayloadPreservesOriginAndForm(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("card callback unexpectedly accessed HTTP") })
	calls := 0
	c.onAction = func(_ context.Context, a core.CardAction) (core.Args, error) {
		calls++
		if a.UserID != "ou_owner" || a.ChatID != "oc_allowed" || a.MessageID != "om_approval" || a.Value["requestId"] != "request_fixture" || a.FormValue["answer"] != "scope" {
			t.Errorf("action boundary: %+v", a)
		}
		return core.Args{"toast": core.Args{"type": "info", "content": "已确认"}}, nil
	}
	payload := []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger"},"event":{"operator":{"open_id":"ou_owner"},"context":{"open_chat_id":"oc_allowed","open_message_id":"om_approval"},"action":{"value":{"requestId":"request_fixture","choice":"approve"},"form_value":{"answer":"scope"}}}}`)
	result, err := c.HandleEvent(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if calls != 1 || !strings.Contains(string(encoded), "已确认") {
		t.Fatal("missing callback result")
	}
	payload = []byte(strings.ReplaceAll(string(payload), "ou_owner", "ou_stranger"))
	_, err = c.HandleEvent(context.Background(), payload)
	if err != nil || calls != 1 {
		t.Fatal("unauthorized callback reached gateway")
	}
}
func TestStandaloneImageAndDMTrigger(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected HTTP") })
	var received core.Message
	c.onMessage = func(_ context.Context, m core.Message) error { received = m; return nil }
	payload := strings.ReplaceAll(string(rawEvent("oc_dm", "ou_dm", "image", `{"image_key":"img_fixture"}`, false)), `"chat_type":"group"`, `"chat_type":"p2p"`)
	if _, err := c.HandleEvent(context.Background(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if !received.ImageOnly || received.Mentioned || received.ChatType != "p2p" {
		t.Fatalf("DM image event: %+v", received)
	}
}
func TestMentionOptionalGroupAcceptsUnmentionedMessages(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected HTTP") })
	c.config.Groups[0].MentionOptional = true
	var received []core.Message
	c.onMessage = func(_ context.Context, m core.Message) error { received = append(received, m); return nil }
	for _, event := range []struct {
		kind    string
		content string
	}{
		{"text", `{"text":"hello"}`},
		{"image", `{"image_key":"img_fixture"}`},
	} {
		if _, err := c.HandleEvent(context.Background(), rawEvent("oc_allowed", "ou_member", event.kind, event.content, false)); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 2 || received[0].Mentioned || received[0].Text != "hello" || !received[1].ImageOnly {
		t.Fatalf("mention-optional events: %+v", received)
	}
}
func TestMentionOptionalGroupStillRejectsUnauthorizedAuthor(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected HTTP") })
	c.config.Groups[0].MentionOptional = true
	called := false
	c.onMessage = func(context.Context, core.Message) error { called = true; return nil }
	if _, err := c.HandleEvent(context.Background(), rawEvent("oc_allowed", "ou_stranger", "text", `{"text":"hello"}`, false)); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unauthorized author reached gateway")
	}
}

func TestDefaultGroupBindingAcceptsMaintainerOnly(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected HTTP") })
	c.config.Groups = append(c.config.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "fixture"})
	var received []core.Message
	c.onMessage = func(_ context.Context, m core.Message) error { received = append(received, m); return nil }
	for _, event := range []struct {
		user      string
		mentioned bool
	}{
		{"ou_owner", true},
		{"ou_member", true},
		{"ou_owner", false},
	} {
		if _, err := c.HandleEvent(context.Background(), rawEvent("oc_unlisted", event.user, "text", `{"text":"hello"}`, event.mentioned)); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 1 || received[0].SenderID != "ou_owner" || received[0].ChatID != "oc_unlisted" {
		t.Fatalf("default group accepted unexpected events: %+v", received)
	}
}

func TestDefaultGroupBindingCanAllowAllMembers(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected HTTP") })
	c.config.Groups = append(c.config.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "fixture", AllowAllMembers: true})
	var received []core.Message
	c.onMessage = func(_ context.Context, m core.Message) error { received = append(received, m); return nil }
	if _, err := c.HandleEvent(context.Background(), rawEvent("oc_unlisted", "ou_stranger", "text", `{"text":"hello"}`, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HandleEvent(context.Background(), rawEvent("oc_unlisted", "ou_stranger", "text", `{"text":"hello"}`, false)); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].SenderID != "ou_stranger" || !received[0].Mentioned {
		t.Fatalf("allow-all default group: %+v", received)
	}
}

func TestExplicitGroupBindingOverridesDefaultPolicy(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected HTTP") })
	c.config.Groups = append(c.config.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "fixture", AllowAllMembers: true})
	var received []core.Message
	c.onMessage = func(_ context.Context, m core.Message) error { received = append(received, m); return nil }
	for _, user := range []string{"ou_member", "ou_stranger"} {
		if _, err := c.HandleEvent(context.Background(), rawEvent("oc_allowed", user, "text", `{"text":"hello"}`, true)); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 1 || received[0].SenderID != "ou_member" {
		t.Fatalf("explicit binding inherited default policy: %+v", received)
	}
}

func TestInteractiveHistoryCardDegradedContent(t *testing.T) {
	// CardKit history content carries only the card entity; clients without the
	// rendered entity fall back to no text or this public placeholder.
	cardEntity := map[string]any{"type": "card", "data": map[string]any{"card_id": "card_example"}}
	if text, ok := renderInteractive(cardEntity); !ok || text != "" {
		t.Fatalf("card entity content = %q, %t", text, ok)
	}
	placeholder := map[string]any{"body": map[string]any{"elements": []any{
		map[string]any{"tag": "markdown", "content": core.HistoryCardPlaceholder},
	}}}
	if text, ok := renderInteractive(placeholder); !ok || text != core.HistoryCardPlaceholder {
		t.Fatalf("degraded card content = %q, %t", text, ok)
	}
}

func TestOtherBotEventHeaderAdmission(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {})
	c.config.Groups[0].AllowOtherBots = true
	event := messageEvent{}
	event.Sender.Type = "app"
	event.Sender.Name = "Helper"
	event.Sender.ID.AppID = "cli_helper"
	event.Message = eventMessage{ID: "om_bot", ChatID: "oc_allowed", ChatType: "group", Type: "text", Content: `{"text":"hello"}`}
	room, ok := c.allowedHeader(event)
	if !ok || !room.AllowOtherBots {
		t.Fatalf("other bot event was rejected: %+v %t", room, ok)
	}
	m := normalizeMessage(event, c.botOpenID(), true)
	if m.SenderID != "cli_helper" || !m.SenderIsBot || m.SenderIsSelf {
		t.Fatalf("other bot sender = %+v", m)
	}
	event.Sender.ID.AppID = c.config.AppID
	if _, ok := c.allowedHeader(event); ok {
		t.Fatal("self app event was admitted")
	}
	event.Sender.ID.AppID = "cli_helper"
	c.config.Groups[0].AllowOtherBots = false
	if _, ok := c.allowedHeader(event); ok {
		t.Fatal("other bot event was admitted without allowOtherBots")
	}
}
