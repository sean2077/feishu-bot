package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// cardUpdateRejected replays the observed non-retryable CardKit 300301
// rejection from the final cosmetic control refresh.
type cardUpdateRejected struct{}

func (cardUpdateRejected) Error() string {
	return "feishu: card update failed (HTTP 200, code 300301, uncertain false, retryable false)"
}
func (cardUpdateRejected) DeliveryRetryable() bool           { return false }
func (cardUpdateRejected) DeliveryUncertain() bool           { return false }
func (cardUpdateRejected) DeliveryRetryAfter() time.Duration { return 0 }

func TestFinalControlRefreshRejectionLeavesNoRetryOrSecondCard(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	card := replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true)
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "answer", true, "", "", "", card, nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	var updates atomic.Int32
	f.tr.updateHook = func(context.Context, string, core.Args) error {
		if updates.Add(1) >= 2 {
			return cardUpdateRejected{}
		}
		return nil
	}
	ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false)
	if !ok || err != nil {
		t.Fatalf("successful delivery was reported as failed: ok=%t err=%v", ok, err)
	}
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&state); err != nil || state != "done" {
		t.Fatalf("outbox state = %q, %v", state, err)
	}
	f.tr.mu.Lock()
	created, updatesCopy := len(f.tr.cards), append([]sentCard{}, f.tr.updates...)
	f.tr.mu.Unlock()
	if created != 1 {
		t.Fatalf("delivery created %d cards, want one", created)
	}
	if len(updatesCopy) < 2 {
		t.Fatalf("only %d card updates, want the final refresh attempt", len(updatesCopy))
	}
	for _, update := range updatesCopy {
		if strings.Contains(jsonText(update.card), "重试交付") {
			t.Fatalf("card update exposed retry after successful delivery: %s", jsonText(update.card))
		}
	}
	var audits int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='output_controls_refresh_failed'", f.room().Key).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("refresh-failure audit = %d, %v", audits, err)
	}
}
