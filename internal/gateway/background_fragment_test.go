package gateway

import (
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

// A native turn can carry output attributed to background work (for example
// a task notification absorbed mid-turn) and still end with one foreground
// result. That result is the only one the turn produces, so every other card
// opened in the turn must be finalized instead of streaming forever.
func TestForegroundResultFinalizesBackgroundCardsOfTheSameTurn(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("human", "record this", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "thinking", TurnID: d.UUID, Source: "foreground", BlockID: "m1:0", Text: "start"})
	eventually(t, func() bool { return f.tr.cardCount() == 1 })
	for i, run := range []string{"run-a", "run-b"} {
		w.emit(core.WorkerEvent{Type: "thinking", TurnID: d.UUID, Source: "background", BackgroundID: run, BlockID: run + ":0", Text: "continue"})
		want := i + 2
		eventually(t, func() bool { return f.tr.cardCount() == want })
	}
	f.finish(w, d, "final answer")
	eventually(t, func() bool { return f.tr.receiptDone("human") })
	eventually(t, func() bool {
		var open int
		err := f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE session=? AND kind='reply' AND state!='done'", d.SessionID).Scan(&open)
		return err == nil && open == 0
	})
	var finals, targets int
	if err := f.s.DB.QueryRow("SELECT count(*),count(DISTINCT target) FROM outbox WHERE session=? AND kind='reply'", d.SessionID).Scan(&finals, &targets); err != nil || finals != 3 || targets != 3 {
		t.Fatal("every card of the turn needs its own final delivery", finals, targets, err)
	}
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	left, touched := len(s.renders), len(s.turnRenders)
	s.mu.Unlock()
	if left != 0 || touched != 0 {
		t.Fatal("renders survived the native turn", left, touched)
	}
	if f.tr.cardCount() != 3 {
		t.Fatal("closing a fragment posted another card", f.tr.cardCount())
	}
}

// A render that never reached the platform is closed without creating a card.
func TestUnsentBackgroundFragmentClosesWithoutNewCard(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("human", "record this", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "thinking", TurnID: d.UUID, Source: "foreground", BlockID: "m1:0", Text: "start"})
	eventually(t, func() bool { return f.tr.cardCount() == 1 })
	// No reply anchor exists for this unknown turn, so no card can be created.
	w.emit(core.WorkerEvent{Type: "thinking", TurnID: "unknown-turn", Source: "background", BackgroundID: "run", BlockID: "run:0", Text: "orphan"})
	f.finish(w, d, "final answer")
	eventually(t, func() bool { return f.tr.receiptDone("human") })
	var replies int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE session=? AND kind='reply'", d.SessionID).Scan(&replies); err != nil || replies != 1 {
		t.Fatal(replies, err)
	}
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	left := len(s.renders)
	s.mu.Unlock()
	if left != 0 || f.tr.cardCount() != 1 {
		t.Fatal("unsent fragment stayed open or posted a card", left, f.tr.cardCount())
	}
}
