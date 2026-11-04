package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestForwardedApprovalUsesMaintainerOpenIDWithoutGrantingDMAccess(t *testing.T) {
	writes := 0
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/open-apis/im/v1/messages" || r.URL.Query().Get("receive_id_type") != "open_id" {
			t.Errorf("incorrect approval destination: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		body := decodedBody(t, r)
		if body["receive_id"] != "ou_owner" || body["msg_type"] != "interactive" {
			t.Errorf("incorrect approval payload: %#v", body)
		}
		writes++
		apiResponse(w, map[string]string{"message_id": "om_approval"})
	})
	origin, ok := roomFor(c.config, "oc_requester_dm", "ou_dm")
	if !ok {
		t.Fatal("missing requester DM")
	}
	target := origin
	target.ChatID, target.DMUser = "ou_owner", "ou_owner"
	card := core.Args{"header": core.Args{"title": core.Args{"tag": "plain_text", "content": "需要维护者批准"}}, "body": core.Args{"elements": []any{core.Args{"tag": "button", "text": core.Args{"tag": "plain_text", "content": "批准一次"}, "value": core.Args{"request": "fixture-request", "decision": "allow"}}}}}
	mid, err := c.Card(context.Background(), target, card, "", "")
	if err != nil || mid != "om_approval" {
		t.Fatalf("approval forwarding failed: %q %v", mid, err)
	}
	// The recipient does not occur in DMs. Their authenticated callback must
	// reach the gateway, which binds this message ID to its pending request.
	payload := []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger"},"event":{"operator":{"open_id":"ou_owner"},"context":{"open_chat_id":"oc_maintainer_dm","open_message_id":"om_approval"},"action":{"value":{"request":"fixture-request","decision":"allow"}}}}`)
	event, err := c.prepareEvent(payload)
	if err != nil || event.action == nil || event.action.UserID != "ou_owner" || event.action.MessageID != mid {
		t.Fatalf("forwarded callback was not delivered: %#v %v", event, err)
	}
	// Forwarding a card must not allow the same synthetic room to become a
	// history/read/send capability or allow a third party to receive a card.
	if _, err = c.Send(context.Background(), target, "sensitive", "", ""); err == nil {
		t.Fatal("forward target granted normal send access")
	}
	if _, err = c.Fetch(context.Background(), "om_private", target); err == nil {
		t.Fatal("forward target granted history access")
	}
	for _, invalid := range []core.Room{
		{Key: origin.Key, ChatID: "ou_stranger", DMUser: "ou_stranger", Maintainers: []string{"ou_stranger"}},
		{Key: fixtureRoom(t, c).Key, ChatID: "ou_owner", DMUser: "ou_owner"},
		{Key: "forged", ChatID: "ou_owner", DMUser: "ou_owner"},
	} {
		if _, err = c.Card(context.Background(), invalid, card, "", ""); err == nil {
			t.Fatalf("accepted unauthorized approval destination: %#v", invalid)
		}
	}
	if _, err = c.Card(context.Background(), target, card, "om_private", ""); err == nil {
		t.Fatal("forward target accepted a reply origin")
	}
	if writes != 1 {
		t.Fatalf("unexpected writes: %d", writes)
	}
	// The maintainer exception is callback-only: ordinary incoming DM text is
	// still gated by the explicit DMs configuration.
	var raw map[string]any
	_ = json.Unmarshal(rawEvent("oc_maintainer_dm", "ou_owner", "text", `{"text":"hello"}`, false), &raw)
	raw["event"].(map[string]any)["message"].(map[string]any)["chat_type"] = "p2p"
	b, _ := json.Marshal(raw)
	event, err = c.prepareEvent(b)
	if err != nil || event.message != nil {
		t.Fatal("approval recipient gained ordinary DM access")
	}
}
