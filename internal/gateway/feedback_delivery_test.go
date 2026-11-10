package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestDeliveredFeedbackCardHasOneControlSet(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Projects[0].Feedback.Enabled = true })
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	card := replyCard("回复", "answer", nil, 0, true, replyDetails{}, 0, true)
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "answer", true, "", "", "", card, nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false)
	if !ok || err != nil {
		t.Fatalf("delivery = %t %v", ok, err)
	}
	f.tr.mu.Lock()
	updates := append([]sentCard{}, f.tr.updates...)
	f.tr.mu.Unlock()
	found := 0
	for _, update := range updates {
		text := jsonText(update.card)
		ids := map[string]int{}
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if id, ok := x["element_id"].(string); ok && id != "" {
					ids[id]++
				}
				for _, child := range x {
					walk(child)
				}
			case []any:
				for _, child := range x {
					walk(child)
				}
			}
		}
		walk(update.card)
		for id, n := range ids {
			if n > 1 {
				t.Errorf("update duplicate %s: %s", id, text)
			}
			if len(id) > 20 {
				t.Errorf("element_id %q exceeds CardKit's 20-character limit", id)
			}
		}
		if strings.Contains(text, "gateway_quick_fb_yes") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("feedback controls appeared in %d of %d updates", found, len(updates))
	}
	if len(updates) == 0 || !strings.Contains(jsonText(updates[len(updates)-1].card), "gateway_quick_fb_yes") {
		t.Fatal("feedback controls were not part of the final successful update")
	}
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&state); err != nil || state != "done" {
		t.Fatalf("outbox state = %q, %v", state, err)
	}
}
