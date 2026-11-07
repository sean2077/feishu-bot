package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestTopicRootFetchFailureIsRetriedAfterAnOrdinaryInput(t *testing.T) {
	f := topicFixture(t)
	root := f.message("human-topic", "the release plan", "member")
	root.Mentioned = false
	var attempts atomic.Int64
	f.tr.fetchHook = func(context.Context, string, core.Room) (*core.Message, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("temporary Fetch failure")
		}
		return &root, nil
	}
	f.send(inTopic(f.message("first", "look at this", "second"), root.ID, "omt_human"))
	w := f.worker()
	first := f.delivery(w)
	if first.Quote != nil {
		t.Fatal("failed Fetch supplied a quote")
	}
	w.emit(core.WorkerEvent{Type: "consumed", UUID: first.UUID})
	f.finish(w, first, "first answer")
	f.send(inTopic(f.message("second", "and check the dates", "second"), root.ID, "omt_human"))
	second := f.delivery(w)
	if second.Quote == nil || second.Quote.ID != root.ID {
		t.Fatalf("an ordinary input suppressed the never-delivered root: %+v", second)
	}
	room := f.topic(root.ID)
	if delivered, err := f.s.TopicRootDelivered(context.Background(), room.Key, second.SessionID, root.ID); err != nil || delivered {
		t.Fatalf("sending alone was treated as durable native confirmation: %v %v", delivered, err)
	}
	w.emit(core.WorkerEvent{Type: "consumed", UUID: second.UUID})
	if delivered, err := f.s.TopicRootDelivered(context.Background(), room.Key, second.SessionID, root.ID); err != nil || !delivered {
		t.Fatalf("native consumption did not confirm the root: %v %v", delivered, err)
	}
	f.finish(w, second, "second answer")
	if err := f.g.stopRoom(context.Background(), room); err != nil {
		t.Fatal(err)
	}
	f.send(inTopic(f.message("third", "continue after process reclamation", "second"), root.ID, "omt_human"))
	third := f.delivery(f.worker())
	if third.Quote != nil || third.SessionID != second.SessionID || attempts.Load() != 2 {
		t.Fatalf("confirmed context was re-fetched after reclamation: %+v attempts=%d", third, attempts.Load())
	}
}

func TestUnconfirmedTopicRootCanBeProvidedAfterStop(t *testing.T) {
	f := topicFixture(t)
	root := f.message("human-topic", "the project context", "member")
	root.Mentioned = false
	f.tr.messages[root.ID] = root
	f.send(inTopic(f.message("first", "start", "second"), root.ID, "omt_human"))
	w := f.worker()
	first := f.delivery(w)
	if first.Quote == nil {
		t.Fatal("fixture root missing")
	}
	room := f.topic(root.ID)
	if err := f.g.stopRoom(context.Background(), room); err != nil {
		t.Fatal(err)
	}
	f.send(inTopic(f.message("after-stop", "continue", "second"), root.ID, "omt_human"))
	next := f.delivery(f.worker())
	if next.SessionID != first.SessionID || next.Quote == nil || next.Quote.ID != root.ID {
		t.Fatalf("unconfirmed context was silently considered delivered: %+v", next)
	}
}

func TestCompactDoesNotConsumeTheTopicRootQuote(t *testing.T) {
	f := topicFixture(t)
	root := f.message("human-topic", "a discussion without the bot", "member")
	root.Mentioned = false
	f.tr.messages[root.ID] = root
	f.send(inTopic(f.message("compact", "/compact", "owner"), root.ID, "omt_human"))
	w := f.worker()
	compact := f.delivery(w)
	if compact.Command == nil || compact.Quote != nil {
		t.Fatalf("compact was not a native control input: %+v", compact)
	}
	w.emit(core.WorkerEvent{Type: "consumed", UUID: compact.UUID})
	w.emit(core.WorkerEvent{Type: "result", UUID: compact.UUID, UUIDs: []string{compact.UUID}, ResultID: core.NewID(), Success: true, Compact: "not_needed", Source: "foreground"})
	f.send(inTopic(f.message("first-human", "help with the plan", "second"), root.ID, "omt_human"))
	first := f.delivery(w)
	if first.Quote == nil || first.Quote.ID != root.ID {
		t.Fatalf("compact prevented the first real input from receiving its root: %+v", first)
	}
	f.finish(w, first, "root delivered")
	if delivered, err := f.s.TopicRootDelivered(context.Background(), f.topic(root.ID).Key, first.SessionID, root.ID); err != nil || !delivered {
		t.Fatalf("final result did not confirm its root without a separate consumed event: %v %v", delivered, err)
	}
}

func TestImageBatchConfirmsItsSingleRootQuote(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Limits.ImageBatchMS = 5000
		c.Limits.ImageBatchMaxImages = 2
	})
	root := f.message("human-topic", "compare these diagrams", "member")
	root.Mentioned = false
	f.tr.messages[root.ID] = root
	f.send(inTopic(f.image("image-one", "second"), root.ID, "omt_human"))
	f.send(inTopic(f.image("image-two", "second"), root.ID, "omt_human"))
	w := f.worker()
	batch := f.delivery(w)
	quotes := 0
	for _, input := range batch.BatchInputs {
		if input.Quote != nil && input.Quote.ID == root.ID {
			quotes++
		}
	}
	if quotes != 1 {
		t.Fatalf("one native batch needs exactly one root quote, got %d", quotes)
	}
	w.emit(core.WorkerEvent{Type: "consumed", UUID: batch.UUID})
	if delivered, err := f.s.TopicRootDelivered(context.Background(), f.topic(root.ID).Key, batch.SessionID, root.ID); err != nil || !delivered {
		t.Fatalf("batch consumption did not confirm its root: %v %v", delivered, err)
	}
}
