package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestTopicAndCardAdmissionReserveOneChatBudget(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Limits.InboxPerRoom = 1
	})
	chat := f.room()
	var deliveries []core.Delivery
	for i := 0; i < 12; i++ {
		m := f.message(fmt.Sprintf("root_%d", i), "work", "member")
		r, ok := Route(f.c, m)
		if !ok {
			t.Fatal("topic did not route")
		}
		s, err := f.s.Session(r.Key)
		if err != nil {
			t.Fatal(err)
		}
		d := core.Delivery{UUID: core.NewID(), RoomKey: r.Key, SessionID: s.ID, Message: m}
		if i%2 != 0 {
			d.Message.ID = "card:" + m.ID
			d.Message.InteractionMessageID = "bound-card"
			d.SupplementTo = "current-turn"
		}
		deliveries = append(deliveries, d)
	}
	start := make(chan struct{})
	results := make(chan error, len(deliveries))
	var wg sync.WaitGroup
	for _, d := range deliveries {
		wg.Add(1)
		go func(d core.Delivery) {
			defer wg.Done()
			<-start
			results <- f.g.queueInput(context.Background(), d, time.Now().UnixMilli())
		}(d)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, full := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, errInboxFull):
			full++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || full != 11 {
		t.Fatalf("concurrent topics exceeded the shared budget: accepted=%d full=%d", accepted, full)
	}
	if n, err := f.s.CountPendingChat(chat.Key); err != nil || n != 1 {
		t.Fatalf("pending inputs=%d err=%v", n, err)
	}
	// Re-observing an already reserved input does not consume another slot.
	for _, d := range deliveries {
		seen, err := f.s.Seen(d.RoomKey, d.Message.ID)
		if err != nil {
			t.Fatal(err)
		}
		if seen {
			if err := f.g.queueInput(context.Background(), d, time.Now().UnixMilli()); err != nil {
				t.Fatalf("duplicate reservation consumed another slot: %v", err)
			}
		}
	}
	other, _ := RoomFor(f.c, "other", "")
	s, err := f.s.Session(other.Key)
	if err != nil {
		t.Fatal(err)
	}
	m := f.message("other-input", "independent chat", "member")
	m.ChatID = other.ChatID
	if err := f.g.queueInput(context.Background(), core.Delivery{UUID: core.NewID(), RoomKey: other.Key, SessionID: s.ID, Message: m}, time.Now().UnixMilli()); err != nil {
		t.Fatalf("one full chat blocked another chat: %v", err)
	}
}
