package gateway

import (
	"context"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// Approval waits can last longer than a platform stream lease. Pause existing
// reply cards without finishing their turn; resume the same message afterwards.
func (g *Gateway) pauseReplies(room, session, kind string) func() {
	s := g.getSlot(room)
	if s == nil { return func() {} }
	s.mu.Lock()
	var renders []*render
	if s.session.ID == session {
		for _, r := range s.renders { renders = append(renders, r) }
	}
	s.mu.Unlock()
	title := "等待审批"
	if kind == "question" { title = "等待回答" }
	for _, r := range renders { r.waitForAnswer(true, title) }
	return func() { for _, r := range renders { r.waitForAnswer(false, title) } }
}

func (g *Gateway) retainCards(ctx context.Context, now int64) {
	retainer, ok := g.transport.(core.CardRetainer)
	if !ok { return }
	var protected []string
	for _, s := range g.allSlots() {
		s.mu.Lock()
		var renders []*render
		for _, r := range s.renders { renders = append(renders, r) }
		s.mu.Unlock()
		for _, r := range renders {
			r.mu.Lock()
			protected = append(protected, r.messageID, r.deliveryID)
			r.mu.Unlock()
		}
	}
	g.approvalMu.Lock()
	for _, a := range g.approvals { for message := range a.cards { protected = append(protected, message) } }
	g.approvalMu.Unlock()
	// The adapter checks durable outbox references inside its deletion
	// transaction; copying the whole backlog here would defeat bounded cleanup.
	count, err := retainer.Retain(ctx, time.UnixMilli(now).Add(-30*24*time.Hour), protected)
	if err != nil { g.audit("", "card_retention_failed", core.Args{}); return }
	if count > 0 { g.audit("", "card_records_pruned", core.Args{"count": count}) }
}

// The card is already a private copy. Keep all controls and metadata, shrinking
// only the answer to make room for a delivery-status notice.
func fitNoticeCard(card core.Args) core.Args {
	return fitCardBudget(card, core.CardPayloadLimit, "…")
}

func fitCardBudget(card core.Args, budget int, suffix string) core.Args {
	body := argsOf(card["body"])
	elements, _ := body["elements"].([]any)
	var answer core.Args
	for _, item := range elements {
		e := argsOf(item)
		if stringArg(e, "element_id") == "reply_answer" { answer = e; break }
		if answer == nil && stringArg(e, "tag") == "markdown" { answer = e }
	}
	if answer == nil { return card }
	original := stringArg(answer, "content")
	size, err := core.CardPayloadBytes(card)
	if err == nil && size <= budget { return card }
	lo, hi := 0, min(len(original), core.CardPayloadLimit)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		answer["content"] = bytePrefix(original, mid) + suffix
		size, err = core.CardPayloadBytes(card)
		if err == nil && size <= budget { lo = mid } else { hi = mid-1 }
	}
	answer["content"] = bytePrefix(original, lo) + suffix
	return card
}

func (g *Gateway) withOutputControls(card core.Args, room core.Room, session, output string, attempt int) core.Args {
	return replaceQuickControls(card, g.quickOutputControls(room, session, output, attempt))
}
