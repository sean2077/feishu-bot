package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// Occupy the real store's single connection until the operation has queued for
// it, then expire the callback. The result must arrive before storage is freed.
func cancelWhileStorageWaits(t *testing.T, f *fixture, call func(context.Context) error) {
	t.Helper()
	conn, err := f.s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := f.s.DB.Stats().WaitCount
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	eventually(t, func() bool { return f.s.DB.Stats().WaitCount > before })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expired storage wait returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback continued waiting for storage after cancellation")
	}
}

func TestQuickCallbacksDoNotClaimAfterStorageDeadline(t *testing.T) {
	for _, action := range []string{"stop", "resume", "retry", "supplement"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t, nil)
			r := f.room()
			old, err := f.s.Session(r.Key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.NewSession(r.Key); err != nil {
				t.Fatal(err)
			}
			f.send(f.message("work", "work", "member"))
			w := f.worker()
			d := f.delivery(w)
			output, err := f.g.saveOutput(f.s.DB, r, d.SessionID, "saved", true, "", "work", "", nil, origin(d.Message), "", "", "reply", "blocked")
			if err != nil {
				t.Fatal(err)
			}
			p := quickControl{Room: r.Key, Session: d.SessionID, Revision: "fixture-revision", Turn: d.UUID, Output: output, TargetSession: old.ID, Action: action}
			value := core.Args{"quick_control": f.g.signQuick(p)}
			card := DisplayCard("fixture", "callback", core.Args{"tag": "button", "value": value})
			if err = f.g.bindQuickControls("bound-card", card); err != nil {
				t.Fatal(err)
			}
			a := core.CardAction{ChatID: r.ChatID, UserID: "owner", MessageID: "bound-card", Value: value, FormValue: core.Args{"quick_supplement": "more context"}}
			cancelWhileStorageWaits(t, f, func(ctx context.Context) error {
				_, err := f.g.Action(ctx, a)
				return err
			})
			var uses, supplements int
			if err = f.s.DB.QueryRow("SELECT count(*) FROM card_control_uses").Scan(&uses); err != nil {
				t.Fatal(err)
			}
			if err = f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE id LIKE 'card:%'").Scan(&supplements); err != nil {
				t.Fatal(err)
			}
			current, err := f.s.Session(r.Key)
			if err != nil || current.ID != d.SessionID || uses != 0 || supplements != 0 || w.stops.Load() != 0 {
				t.Fatalf("expired callback changed state: session=%s uses=%d supplements=%d stops=%d err=%v", current.ID, uses, supplements, w.stops.Load(), err)
			}
			if o, err := f.g.output(output); err != nil || o.State != "blocked" {
				t.Fatalf("expired retry sent output: %+v %v", o, err)
			}
			if !f.g.quickBound(context.Background(), a, p) {
				t.Fatal("expired callback consumed the live control")
			}
		})
	}
}

func TestQuickClaimAndInputTransactionRespectStorageDeadline(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	p := quickControl{Room: r.Key, Session: s.ID, Revision: "fixture", Action: "stop", Turn: "turn"}
	a := core.CardAction{MessageID: "bound", ChatID: r.ChatID, UserID: "owner"}
	if err = f.g.bindQuickControls(a.MessageID, DisplayCard("fixture", "claim", core.Args{"quick_control": f.g.signQuick(p)})); err != nil {
		t.Fatal(err)
	}
	cancelWhileStorageWaits(t, f, func(ctx context.Context) error {
		claimed, err := f.g.claimQuick(ctx, a, p)
		if claimed {
			t.Error("expired callback claimed the control")
		}
		return err
	})
	d := core.Delivery{UUID: core.NewID(), RoomKey: r.Key, SessionID: s.ID, Message: f.message("card:late", "late supplement", "member"), SupplementTo: "turn"}
	cancelWhileStorageWaits(t, f, func(ctx context.Context) error { return f.g.queueInput(ctx, d, time.Now().UnixMilli()) })
	if seen, err := f.s.Seen(r.Key, d.Message.ID); err != nil || seen {
		t.Fatalf("expired input transaction committed: seen=%v err=%v", seen, err)
	}
	if claimed, err := f.g.claimQuick(context.Background(), a, p); err != nil || !claimed {
		t.Fatalf("uncommitted claim could not be retried: %v %v", claimed, err)
	}
}

func TestApprovalCallbackDeadlineLeavesDecisionPending(t *testing.T) {
	for _, decision := range []string{"allow", "deny", "submit"} {
		t.Run(decision, func(t *testing.T) {
			f := newFixture(t, nil)
			var questions []question
			if decision == "submit" {
				questions = []question{{Question: "Pick", Options: []questionOption{{Label: "A"}}}}
			}
			id, p := pendingApprovalFixture(t, f, questions)
			if !f.g.bindApprovalCard(id, p, "approval-card", p.room) {
				t.Fatal("approval card was not bound")
			}
			a := core.CardAction{UserID: "owner", ChatID: p.room.ChatID, MessageID: "approval-card", Value: core.Args{"request": id, "decision": decision, "session": p.session, "revision": p.revision}, FormValue: core.Args{"question_0": "0"}}
			cancelWhileStorageWaits(t, f, func(ctx context.Context) error {
				_, err := f.g.Action(ctx, a)
				return err
			})
			var state string
			if err := f.s.DB.QueryRow("SELECT state FROM requests WHERE id=?", id).Scan(&state); err != nil || state != "pending" {
				t.Fatalf("expired callback settled request: %s %v", state, err)
			}
			select {
			case result := <-p.outcome:
				t.Fatalf("expired callback answered native request: %v", result)
			default:
			}
			if text, err := f.g.Action(context.Background(), a); err != nil || !strings.Contains(text, "已") {
				t.Fatalf("live request could not be retried: %q %v", text, err)
			}
			select {
			case result := <-p.outcome:
				if boolArg(result, "approved") != (decision != "deny") {
					t.Fatalf("retry settled the wrong decision: %v", result)
				}
			case <-time.After(time.Second):
				t.Fatal("accepted retry did not reach native request")
			}
		})
	}
}

func TestCommittedApprovalSurvivesCallbackCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, nil, func(event string, _ core.Args) {
		if event == "request_settled" {
			cancel() // After the transaction and native outcome, before return.
		}
	})
	id, p := pendingApprovalFixture(t, f, nil)
	if !f.g.bindApprovalCard(id, p, "approval-card", p.room) {
		t.Fatal("approval card was not bound")
	}
	text, err := f.g.Action(ctx, core.CardAction{UserID: "owner", ChatID: p.room.ChatID, MessageID: "approval-card", Value: core.Args{"request": id, "decision": "allow"}})
	if err != nil || !strings.Contains(text, "已批准") || ctx.Err() == nil {
		t.Fatalf("committed decision was lost: %q %v callback=%v", text, err, ctx.Err())
	}
	if result := <-p.outcome; !boolArg(result, "approved") {
		t.Fatalf("committed approval did not reach native: %v", result)
	}
}
