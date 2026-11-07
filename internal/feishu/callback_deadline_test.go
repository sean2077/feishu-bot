package feishu

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestExpiredCardCallbackIsNotHandled(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("unexpected HTTP") })
	calls := 0
	c.onAction = func(context.Context, core.CardAction) (core.Args, error) { calls++; return core.Args{}, nil }
	event := pendingEvent{action: &core.CardAction{UserID: "ou_member", ChatID: "oc_allowed", MessageID: "om_card", Value: core.Args{}}}
	if _, err := c.runEvent(context.Background(), event, time.Now().Add(-3*time.Second)); err == nil || calls != 0 {
		t.Fatalf("expired callback handled: calls=%d err=%v", calls, err)
	}
	if _, err := c.runEvent(context.Background(), event, time.Now()); err != nil || calls != 1 {
		t.Fatalf("fresh callback: calls=%d err=%v", calls, err)
	}
}
