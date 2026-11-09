package native

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func fetchContext(t *testing.T, relay *actionRelay, token string) (map[string]any, int) {
	t.Helper()
	req, err := http.NewRequest("GET", relay.url+"context", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value map[string]any
	if response.StatusCode == 200 {
		if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	return value, response.StatusCode
}

func TestActionRelayContextEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := &core.RequestContext{ChatID: "oc_example", ChatType: "group", ThreadID: "om_thread", MessageID: "om_message", SenderOpenID: "ou_example", SenderName: "Example User", Senders: []string{"ou_example"}, MessageIDs: []string{"om_message"}}
	relay, err := startActionRelay(ctx, nil, func(context.Context, string, args) args { return args{"ok": false} }, func() *core.RequestContext { return current })
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	value, status := fetchContext(t, relay, relay.token)
	if status != 200 || value["active"] != true || value["chatId"] != "oc_example" || value["messageId"] != "om_message" || value["senderOpenId"] != "ou_example" {
		t.Fatalf("context response = %#v (status %d)", value, status)
	}
	if _, status = fetchContext(t, relay, "wrong-token"); status != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", status)
	}

	inactive, err := startActionRelay(ctx, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer inactive.close()
	value, status = fetchContext(t, inactive, inactive.token)
	if status != 200 || value["active"] != false {
		t.Fatalf("inactive context = %#v (status %d)", value, status)
	}
}

func TestActionRelayContextDoesNotCrossWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := startActionRelay(ctx, nil, nil, func() *core.RequestContext {
		return &core.RequestContext{ChatID: "oc_first", Senders: []string{}, MessageIDs: []string{}}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := startActionRelay(ctx, nil, nil, func() *core.RequestContext {
		return &core.RequestContext{ChatID: "oc_second", Senders: []string{}, MessageIDs: []string{}}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if value, _ := fetchContext(t, first, first.token); value["chatId"] != "oc_first" {
		t.Fatalf("first relay = %#v", value)
	}
	if value, _ := fetchContext(t, second, second.token); value["chatId"] != "oc_second" {
		t.Fatalf("second relay = %#v", value)
	}
	if _, status := fetchContext(t, first, second.token); status != http.StatusUnauthorized {
		t.Fatalf("second token read the first relay with status %d", status)
	}
}

func TestRunContextClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay, err := startActionRelay(ctx, nil, nil, func() *core.RequestContext {
		return &core.RequestContext{ChatID: "oc_example", MessageID: "om_message", SenderOpenID: "ou_example", Senders: []string{"ou_example"}, MessageIDs: []string{"om_message"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()
	t.Setenv("FEISHU_ACTION_URL", relay.url)
	t.Setenv("FEISHU_ACTION_TOKEN", relay.token)
	var out bytes.Buffer
	if err := RunContextClient(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["active"] != true || value["chatId"] != "oc_example" {
		t.Fatalf("client context = %#v", value)
	}
	t.Setenv("FEISHU_ACTION_TOKEN", "wrong-token")
	if err := RunContextClient(context.Background(), &bytes.Buffer{}); err == nil {
		t.Fatal("wrong token was accepted")
	}
}

func TestWorkerStartsContextRelayWithoutActions(t *testing.T) {
	f := newFixture(t)
	f.config.Projects[0].Actions = nil
	f.start()
	w := f.worker.(*worker)
	if w.relay == nil {
		t.Fatal("worker without actions did not start the context relay")
	}
	env := strings.Join(w.cmd.Env, "\n")
	for _, key := range []string{"FEISHU_ACTION_URL=", "FEISHU_ACTION_TOKEN=", "FEISHU_ACTION_CLIENT="} {
		if !strings.Contains(env, key) {
			t.Fatalf("worker environment omitted %s", key)
		}
	}
	w.SetRequestContext(func() *core.RequestContext {
		return &core.RequestContext{ChatID: "oc_example", MessageID: "om_message", SenderOpenID: "ou_example", Senders: []string{"ou_example"}, MessageIDs: []string{"om_message"}}
	})
	t.Setenv("FEISHU_ACTION_URL", w.relay.url)
	t.Setenv("FEISHU_ACTION_TOKEN", w.relay.token)
	var out bytes.Buffer
	if err := RunContextClient(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"chatId":"oc_example"`) {
		t.Fatalf("worker context = %s", out.String())
	}
}
