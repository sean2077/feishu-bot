package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestSessionResumeCardRejectsWrongRoleScopeAndStaleSession(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	_, _ = f.s.Session(r.Key)
	current, _ := f.s.NewSession(r.Key)
	card, selected, err := f.g.sessionsCard(r, "owner", "sessions")
	if err != nil || card == nil || selected != current.ID {
		t.Fatal("missing session controls", err)
	}
	if err := f.g.bindQuickControls("sessions-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "member", MessageID: "sessions-card", Value: testQuickValue(t, f.g, card, "resume")}
	if text, err := f.g.Action(context.Background(), a); err != nil || !strings.Contains(text, "维护者") {
		t.Fatal("member could resume shared session", text, err)
	}
	a.UserID = "owner"
	a.ChatID = "other"
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	actual, _ := f.s.Session(r.Key)
	if actual.ID != current.ID {
		t.Fatal("cross-chat callback changed selected session")
	}
	a.ChatID = r.ChatID
	newer, _ := f.s.NewSession(r.Key)
	if text, err := f.g.Action(context.Background(), a); err != nil || !strings.Contains(text, "会话已切换") {
		t.Fatal("stale session card was not rejected", text, err)
	}
	actual, _ = f.s.Session(r.Key)
	if actual.ID != newer.ID || f.starts.Load() != 0 {
		t.Fatal("stale card changed session or woke model")
	}
	other, _ := RoomFor(f.c, "other", "")
	foreign, _ := f.s.Session(other.Key)
	p := quickControl{Room: r.Key, Session: newer.ID, TargetSession: foreign.ID, Revision: "cross-room-fixture", Action: "resume"}
	value := core.Args{"quick_control": f.g.signQuick(p)}
	foreignCard := DisplayCard("fixture", "fixture", button("resume", value, "default"))
	if err := f.g.bindQuickControls("foreign-target-card", foreignCard); err != nil {
		t.Fatal(err)
	}
	a.MessageID = "foreign-target-card"
	a.Value = value
	if text, err := f.g.Action(context.Background(), a); err != nil || !strings.Contains(text, "不属于") {
		t.Fatal("signed foreign target was accepted", text, err)
	}
	actual, _ = f.s.Session(r.Key)
	if actual.ID != newer.ID {
		t.Fatal("foreign target changed current session")
	}
}

func TestSessionResumeCardStopsCurrentGenerationWithoutStartingModel(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	old, _ := f.s.Session(r.Key)
	nativeID := core.NewID()
	if err := f.s.SetCC(old.ID, nativeID); err != nil {
		t.Fatal(err)
	}
	_, _ = f.s.NewSession(r.Key)
	f.send(f.message("active", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	card, _, err := f.g.sessionsCard(r, "owner", "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.bindQuickControls("sessions-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "owner", MessageID: "sessions-card", Value: testQuickValue(t, f.g, card, "resume")}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		current, err := f.s.Session(r.Key)
		return err == nil && current.ID == old.ID && f.g.getSlot(r.Key) == nil
	})
	current, _ := f.s.Session(r.Key)
	if current.CCID != nativeID || f.starts.Load() != 1 || w.stops.Load() != 1 {
		t.Fatal("resume replaced identity or started model before input", current)
	}
}

func TestQuickRetryRevisionChangesAfterFailedAttempt(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	first := DisplayCard("pending", "reply", f.g.quickOutputControls(r, s.ID, "saved-output", 1)...)
	if err := f.g.bindQuickControls("output-card", first); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "owner", MessageID: "output-card", Value: testQuickValue(t, f.g, first, "retry")}
	p, _ := f.g.readQuick(stringArg(a.Value, "quick_control"))
	if ok, err := f.g.claimQuick(context.Background(), a, p); err != nil || !ok {
		t.Fatal("first explicit attempt not claimable", err)
	}
	if ok, err := f.g.claimQuick(context.Background(), a, p); err != nil || ok {
		t.Fatal("duplicate explicit attempt claim accepted", err)
	}
	second := replaceQuickControls(first, f.g.quickOutputControls(r, s.ID, "saved-output", 2))
	if err := f.g.bindQuickControls("output-card", second); err != nil {
		t.Fatal(err)
	}
	if f.g.quickBound(context.Background(), a, p) {
		t.Fatal("prior retry attempt remained active")
	}
	b := a
	b.Value = testQuickValue(t, f.g, second, "retry")
	next, _ := f.g.readQuick(stringArg(b.Value, "quick_control"))
	if next.Output != p.Output || next.Revision == p.Revision {
		t.Fatal("retry revision changed output identity instead of attempt")
	}
	if ok, err := f.g.claimQuick(context.Background(), b, next); err != nil || !ok {
		t.Fatal("new visible retry attempt cannot be claimed", err)
	}
}
