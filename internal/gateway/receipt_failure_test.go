package gateway

import (
	"context"
	"testing"
	"time"
)

type fixtureDeliveryFailure struct{ retryable, uncertain bool }

func (f fixtureDeliveryFailure) Error() string                     { return "fixture delivery failure" }
func (f fixtureDeliveryFailure) DeliveryRetryable() bool           { return f.retryable }
func (f fixtureDeliveryFailure) DeliveryUncertain() bool           { return f.uncertain }
func (f fixtureDeliveryFailure) DeliveryRetryAfter() time.Duration { return 0 }

func TestReceiptFailuresConvergeWithoutReplayingUnknownWrites(t *testing.T) {
	cases := []struct {
		name        string
		failure     fixtureDeliveryFailure
		age         time.Duration
		wantApplied string
	}{
		{"definite retryable keeps the prior state", fixtureDeliveryFailure{retryable: true}, 0, "processing"},
		{"definite rejection settles", fixtureDeliveryFailure{}, 0, "done"},
		{"recent uncertain stays reconciling", fixtureDeliveryFailure{uncertain: true}, 0, "attempt:done"},
		{"stale uncertain settles", fixtureDeliveryFailure{uncertain: true}, 2 * time.Hour, "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			r := f.room()
			s, _ := f.s.Session(r.Key)
			at := time.Now().Add(-tc.age).UnixMilli()
			applied := "processing"
			if tc.failure.uncertain {
				applied = "attempt:done"
			}
			if _, e := f.s.DB.Exec("INSERT INTO receipts(id,room,session,desired,applied,at,sender) VALUES('om_receipt',?,?,'done',?,?,'member')", r.Key, s.ID, applied, at); e != nil {
				t.Fatal(e)
			}
			var previous []string
			f.tr.receiptHook = func(_ context.Context, _, _, prior string) error {
				previous = append(previous, prior)
				return tc.failure
			}
			f.g.flushReceipt("om_receipt")
			var got string
			_ = f.s.DB.QueryRow("SELECT applied FROM receipts WHERE id='om_receipt'").Scan(&got)
			if got != tc.wantApplied {
				t.Fatalf("applied=%q want %q (calls %v)", got, tc.wantApplied, previous)
			}
		})
	}
}

func TestDefiniteCreateFailureRetriesAsCreate(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	if _, e := f.s.DB.Exec("INSERT INTO receipts(id,room,session,desired,applied,at,sender) VALUES('om_receipt',?,?,'done','clear',?,'member')", r.Key, s.ID, time.Now().UnixMilli()); e != nil {
		t.Fatal(e)
	}
	var previous []string
	fail := true
	f.tr.receiptHook = func(_ context.Context, _, _, prior string) error {
		previous = append(previous, prior)
		if fail {
			return fixtureDeliveryFailure{retryable: true}
		}
		return nil
	}
	f.g.flushReceipt("om_receipt")
	fail = false
	f.g.flushReceipt("om_receipt")
	var got string
	_ = f.s.DB.QueryRow("SELECT applied FROM receipts WHERE id='om_receipt'").Scan(&got)
	if got != "done" || len(previous) != 2 || previous[1] != "clear" {
		t.Fatalf("retry after definite failure reconciled instead of creating: applied=%q calls=%v", got, previous)
	}
}

func TestRejectedClearStepStillAddsTheDoneReaction(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	if _, e := f.s.DB.Exec("INSERT INTO receipts(id,room,session,desired,applied,at,sender) VALUES('om_receipt',?,?,'done','processing',?,'member')", r.Key, s.ID, time.Now().UnixMilli()); e != nil {
		t.Fatal(e)
	}
	var steps []string
	f.tr.receiptHook = func(_ context.Context, _, state, prior string) error {
		steps = append(steps, state+"<"+prior)
		if state == "clear" {
			return fixtureDeliveryFailure{}
		}
		return nil
	}
	f.g.flushReceipt("om_receipt")
	var got string
	_ = f.s.DB.QueryRow("SELECT applied FROM receipts WHERE id='om_receipt'").Scan(&got)
	if got != "done" || len(steps) != 2 || steps[1] != "done<clear" {
		t.Fatalf("applied=%q steps=%v", got, steps)
	}
}
