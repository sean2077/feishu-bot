package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

// These are raw card.action.trigger bodies using the documented Card 2.0
// form_value shape: strings for input/single-select, arrays for multi-select.
// The local adapter fixture does not certify rendering on a live Feishu client.
func TestQuestionFormRawCallbackPreservesTextAndSelectionTypes(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("form callback unexpectedly contacted HTTP") })
	calls := 0
	c.onAction = func(_ context.Context, a core.CardAction) (core.Args, error) {
		calls++
		if a.UserID != "ou_member" || a.ChatID != "oc_allowed" || a.MessageID != "om_question" || a.Value["request"] != "request" || a.Value["session"] != "session" || a.Value["revision"] != "revision" || a.Value["decision"] != "submit" {
			t.Errorf("form origin or request binding changed: %+v", a)
		}
		if a.FormValue["question_0"] != "1" || a.FormValue["custom_0"] != "  custom\n/approve literal  " || !reflect.DeepEqual(a.FormValue["question_1"], []any{"1", "0"}) || a.FormValue["custom_1"] != "" {
			t.Errorf("form values changed before gateway validation: %#v", a.FormValue)
		}
		return core.Args{"toast": core.Args{"type": "info", "content": "received"}}, nil
	}
	payload := []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger"},"event":{"operator":{"open_id":"ou_member"},"context":{"open_chat_id":"oc_allowed","open_message_id":"om_question"},"action":{"tag":"button","name":"question_submit","value":{"request":"request","session":"session","revision":"revision","decision":"submit"},"form_value":{"question_0":"1","custom_0":"  custom\n/approve literal  ","question_1":["1","0"],"custom_1":""}}}}`)
	for _, encodedValue := range []bool{false, true} {
		raw := payload
		if encodedValue {
			var event core.Args
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatal(err)
			}
			action := event["event"].(map[string]any)["action"].(map[string]any)
			value, _ := json.Marshal(action["value"])
			action["value"] = string(value)
			raw, _ = json.Marshal(event)
		}
		if _, err := c.HandleEvent(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("valid callback count = %d, want 2", calls)
	}
	for _, bad := range []string{strings.ReplaceAll(string(payload), "ou_member", "ou_stranger"), strings.ReplaceAll(string(payload), "oc_allowed", "oc_other")} {
		if _, err := c.HandleEvent(context.Background(), []byte(bad)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("unauthorized form callback reached the gateway handler")
	}
}
