package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func testQuickValue(t *testing.T, g *Gateway, card core.Args, action string) core.Args {
	t.Helper()
	var result core.Args
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if token, ok := x["quick_control"].(string); ok {
				p, valid := g.readQuick(token)
				if valid && p.Action == action {
					result = core.Args{"quick_control": token}
				}
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
	walk(card)
	if result == nil {
		t.Fatal("missing quick control", action)
	}
	return result
}

func TestQuickControlsDeterministicAndRevisionBound(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	controls := f.g.quickControls(r, s.ID, "turn-one", "")
	first, _ := json.Marshal(controls)
	second, _ := json.Marshal(f.g.quickControls(r, s.ID, "turn-one", ""))
	if string(first) != string(second) {
		t.Fatal("streaming quick controls are not deterministic")
	}
	card := DisplayCard("fixture", "reply", controls...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "member", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "status")}
	p, _ := f.g.readQuick(stringArg(a.Value, "quick_control"))
	if !f.g.quickBound(a, p) {
		t.Fatal("successfully delivered card was not bound")
	}
	updated := replaceQuickControls(card, f.g.quickControls(r, s.ID, "turn-two", ""))
	if err := f.g.bindQuickControls("bound-card", updated); err != nil {
		t.Fatal(err)
	}
	if f.g.quickBound(a, p) {
		t.Fatal("old card revision remained active")
	}
	for _, attack := range []core.CardAction{
		{ChatID: "other", UserID: "member", MessageID: "bound-card", Value: testQuickValue(t, f.g, updated, "status")},
		{ChatID: r.ChatID, UserID: "stranger", MessageID: "bound-card", Value: testQuickValue(t, f.g, updated, "status")},
		{ChatID: r.ChatID, UserID: "member", MessageID: "unbound-card", Value: testQuickValue(t, f.g, updated, "status")},
		{ChatID: r.ChatID, UserID: "member", MessageID: "bound-card", Value: core.Args{"quick_control": stringArg(a.Value, "quick_control") + ".tampered"}},
	} {
		if _, err := f.g.Action(context.Background(), attack); err != nil {
			t.Fatal(err)
		}
	}
	f.tr.mu.Lock()
	notices := len(f.tr.notices)
	f.tr.mu.Unlock()
	if notices != 0 || f.starts.Load() != 0 {
		t.Fatal("unauthorized callback had side effects")
	}
	var original core.Args
	_ = json.Unmarshal([]byte(jsonText(card)), &original)
	if stringArg(testQuickValue(t, f.g, original, "status"), "quick_control") != stringArg(a.Value, "quick_control") {
		t.Fatal("control refresh mutated frozen create payload")
	}
}

func TestQuickSupplementDeduplicatesAndOldStopCannotStopLaterTurn(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	card := DisplayCard("fixture", "reply", f.g.quickControls(r, d.SessionID, d.UUID, "")...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "second", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "supplement"), FormValue: core.Args{"quick_supplement": "/new"}}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	extra := f.delivery(w)
	if extra.SupplementTo != d.UUID || extra.SessionID != d.SessionID || extra.Message.Text != "/new" || extra.Message.InteractionMessageID != "bound-card" {
		t.Fatal("form supplement changed command / turn semantics", extra)
	}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE room=? AND id=?", r.Key, extra.Message.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate callback created duplicate input", count, err)
	}
	if err := f.s.DB.QueryRow("SELECT count(*) FROM receipts WHERE id=?", extra.Message.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("synthetic form input created nonexistent Feishu reaction", count, err)
	}
	select {
	case duplicate := <-w.sent:
		t.Fatal("duplicate form input reached native stream", duplicate)
	default:
	}
	stop := a
	stop.Value = testQuickValue(t, f.g, card, "stop")
	if text, err := f.g.Action(context.Background(), stop); err != nil || !strings.Contains(text, "维护者") {
		t.Fatal("ordinary group member could stop shared task", text, err)
	}
	w.emit(core.WorkerEvent{Type: "result", UUID: d.UUID, UUIDs: []string{d.UUID, extra.UUID}, ResultID: core.NewID(), Text: "done", Success: true, Source: "foreground"})
	f.send(f.message("later", "next task", "member"))
	next := f.delivery(w)
	if next.Message.ID != "later" {
		t.Fatal("unexpected input", next)
	}
	stop.UserID = "owner"
	if text, err := f.g.Action(context.Background(), stop); err != nil || !strings.Contains(text, "已经结束") {
		t.Fatal("old stop button did not reject later turn", text, err)
	}
	if w.stops.Load() != 0 {
		t.Fatal("old button stopped a later foreground turn")
	}
}

func TestQuickStopIsAuthorizedOnceAndPreservesSession(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	before, _ := f.s.Session(r.Key)
	card := DisplayCard("fixture", "reply", f.g.quickControls(r, d.SessionID, d.UUID, "")...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "owner", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "stop")}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return f.g.getSlot(r.Key) == nil })
	after, _ := f.s.Session(r.Key)
	if w.stops.Load() != 1 || before.ID != after.ID || before.CCID != after.CCID || f.starts.Load() != 1 {
		t.Fatal("duplicate quick stop changed session or started native generation", before, after, w.stops.Load())
	}
}
