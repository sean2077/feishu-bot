package gateway

import "github.com/sean2077/feishu-bot/internal/core"

func carriesTopicRoot(d core.Delivery, root string) bool {
	return root != "" && d.Command == nil && (d.Message.ID == root || d.Quote != nil && d.Quote.ID == root)
}

func (g *Gateway) topicRootKnown(s *slot) (bool, error) {
	known, err := g.store.TopicRootDelivered(s.ctx, s.room.Key, s.session.ID, s.room.Thread)
	if known || err != nil {
		return known, err
	}
	// Avoid repeating a root already in this process's ordered input stream.
	// This is deliberately not durable confirmation: after an interrupted
	// handoff a subsequent input may provide the root again.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, delivery := range s.deliveries {
		if carriesTopicRoot(delivery, s.room.Thread) {
			return true, nil
		}
	}
	return false, nil
}
