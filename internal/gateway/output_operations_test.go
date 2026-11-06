package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func operationOutput(t *testing.T, f *fixture, session, kind, state, originID, target string) string {
	t.Helper()
	who := []core.Origin{{ID: originID, SenderID: "member"}}
	if originID == "" {
		who = nil
	}
	id, err := f.g.saveOutput(f.s.DB, f.room(), session, "saved", true, target, "", "", DisplayCard("reply", "saved"), who, "", "", kind, state)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDiscardOutputGroupsRequiredFilesAndPreservesOtherTurns(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	file := operationOutput(t, f, s.ID, "file", "blocked", "turn-a", "")
	reply := operationOutput(t, f, s.ID, "reply", "pending", "turn-a", "card-a")
	other := operationOutput(t, f, s.ID, "reply", "blocked", "turn-b", "")
	if _, err = f.s.DB.Exec("INSERT INTO receipts(id,room,session,desired,applied,at,sender) VALUES('turn-a',?,?,'processing','processing',1,'member')", r.Key, s.ID); err != nil {
		t.Fatal(err)
	}
	result, err := f.g.discardOutput(context.Background(), r, file, "owner")
	if err != nil || len(result.IDs) != 2 {
		t.Fatal(result, err)
	}
	for _, id := range []string{file, reply} {
		o, err := f.g.output(id)
		if err != nil || o.State != "cancelled" {
			t.Fatal(o, err)
		}
	}
	if o, err := f.g.output(other); err != nil || o.State != "blocked" {
		t.Fatal("unrelated turn discarded", o, err)
	}
	for _, id := range []string{file, reply} {
		if delivered, err := f.g.deliverOutput(context.Background(), id, r, true); err != nil || delivered {
			t.Fatal("discarded output replayed", id, err)
		}
	}
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='status' AND state='done' AND target='card-a'").Scan(&n) == nil && n == 1
	})
	if f.tr.receiptDone("turn-a") || f.starts.Load() != 0 {
		t.Fatal("discard reported completion or started native")
	}
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	if len(f.tr.updates) != 1 || !strings.Contains(jsonText(f.tr.updates[0].card), "未确认完成") || argsOf(f.tr.updates[0].card["config"])["streaming_mode"] != false {
		t.Fatal(f.tr.updates)
	}
	status, color := replyCardStatus(t, f.tr.updates[0].card)
	if status != "● 已放弃交付 · 未确认完成" || color != "grey" || strings.Contains(jsonText(f.tr.updates[0].card), "quick_control") {
		t.Fatal("discard closure has misleading state or active controls", status, color)
	}
}

func TestDiscardBeforeNativeResultDoesNotStopTaskOrMarkDone(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("active-origin", "continue running", "member"))
	w := f.worker()
	d := f.delivery(w)
	file := operationOutput(t, f, d.SessionID, "file", "uncertain", d.Message.ID, "")
	if _, err := f.g.discardOutput(context.Background(), f.room(), file, "owner"); err != nil {
		t.Fatal(err)
	}
	if w.stops.Load() != 0 || !f.g.active(f.g.getSlot(f.room().Key)) {
		t.Fatal("discard stopped the native task")
	}
	f.finish(w, d, "native still completed")
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE session=? AND kind='reply' AND state='cancelled'", d.SessionID).Scan(&n) == nil && n == 1
	})
	if f.tr.receiptDone(d.Message.ID) || w.stops.Load() != 0 {
		t.Fatal("discarded dependency became Done or stopped native")
	}
	later := operationOutput(t, f, d.SessionID, "reply", "pending", "new-turn", "")
	if o, err := f.g.output(later); err != nil || o.State != "pending" {
		t.Fatal("future independent turn suppressed", o, err)
	}
}

func TestDiscardRejectsActiveOrUnboundGroupsAtomically(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	file := operationOutput(t, f, s.ID, "file", "blocked", "same", "")
	reply := operationOutput(t, f, s.ID, "reply", "pending", "same", "")
	f.g.outMu.Lock()
	f.g.outActive[reply] = true
	f.g.outMu.Unlock()
	if _, err = f.g.discardOutput(context.Background(), f.room(), file, "owner"); !errors.Is(err, errOutputBusy) {
		t.Fatal(err)
	}
	f.g.outMu.Lock()
	delete(f.g.outActive, reply)
	f.g.outMu.Unlock()
	if o, err := f.g.output(file); err != nil || o.State != "blocked" {
		t.Fatal("part of busy group was cancelled", o, err)
	}
	unknown := operationOutput(t, f, s.ID, "file", "uncertain", "", "")
	if _, err = f.g.discardOutput(context.Background(), f.room(), unknown, "owner"); !errors.Is(err, errOutputUnbound) {
		t.Fatal(err)
	}
	if _, err = f.g.discardOutput(context.Background(), f.room(), file, "member"); !errors.Is(err, errOutputNotFound) {
		t.Fatal("non-maintainer discard", err)
	}
}

func TestDiscardRejectsReplyWhileDoneReceiptIsStillSettling(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	file := operationOutput(t, f, s.ID, "file", "blocked", "same", "")
	reply := operationOutput(t, f, s.ID, "reply", "done", "same", "")
	f.g.outMu.Lock()
	f.g.outActive[reply] = true
	f.g.outMu.Unlock()
	defer func() { f.g.outMu.Lock(); delete(f.g.outActive, reply); f.g.outMu.Unlock() }()
	if _, err = f.g.discardOutput(context.Background(), f.room(), file, "owner"); !errors.Is(err, errOutputBusy) {
		t.Fatal("late receipt could overwrite discard", err)
	}
}

func TestDiscardRollsBackAndRetainsUnknownFileEvidence(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	id := operationOutput(t, f, s.ID, "file", "uncertain", "source", "")
	if _, err = f.s.DB.Exec("UPDATE outbox SET file_state='uncertain' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("CREATE TRIGGER reject_discard BEFORE INSERT ON operator_decisions BEGIN SELECT RAISE(ABORT,'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.g.discardOutput(context.Background(), f.room(), id, "owner"); err == nil {
		t.Fatal("failed audit persistence acknowledged")
	}
	if o, err := f.g.output(id); err != nil || o.State != "uncertain" || o.FileState != "uncertain" {
		t.Fatal(o, err)
	}
	if _, err = f.s.DB.Exec("DROP TRIGGER reject_discard"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.g.discardOutput(context.Background(), f.room(), id, "owner"); err != nil {
		t.Fatal(err)
	}
	if o, err := f.g.output(id); err != nil || o.State != "cancelled" || o.FileState != "uncertain" {
		t.Fatal("unknown file was falsely acknowledged", o, err)
	}
}

func TestDiscardLatePreviewClosesExistingCardOnce(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	file := operationOutput(t, f, s.ID, "file", "blocked", "same", "")
	if _, err = f.g.discardOutput(context.Background(), f.room(), file, "owner"); err != nil {
		t.Fatal(err)
	}
	reply := operationOutput(t, f, s.ID, "reply", "preparing", "same", "")
	for i := 0; i < 2; i++ {
		discarded, err := f.g.closeDiscardedOutput(context.Background(), reply, f.room(), "late-card")
		if err != nil || !discarded {
			t.Fatal(discarded, err)
		}
	}
	eventually(t, func() bool {
		var n int
		return f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='status' AND state='done' AND target='late-card'").Scan(&n) == nil && n == 1
	})
	if f.tr.cardCount() != 0 || f.tr.receiptDone("same") {
		t.Fatal("abandonment created a new answer or Done")
	}
}

func TestDiscardClosingStatusFailureRollsBackWholeDecision(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	id := operationOutput(t, f, s.ID, "reply", "blocked", "bound", "existing-card")
	if _, err = f.s.DB.Exec("CREATE TRIGGER reject_discard_status BEFORE INSERT ON outbox WHEN NEW.kind='status' BEGIN SELECT RAISE(ABORT,'fixture closing status failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.g.discardOutput(context.Background(), f.room(), id, "owner"); err == nil {
		t.Fatal("decision committed without its durable closing status")
	}
	if o, err := f.g.output(id); err != nil || o.State != "blocked" {
		t.Fatal("partial cancellation escaped rollback", o, err)
	}
	var n int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM operator_decisions").Scan(&n); err != nil || n != 0 {
		t.Fatal("partial decision escaped rollback", n, err)
	}
}

func TestDiscardRefusesUnattributableRequiredSibling(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	id := operationOutput(t, f, s.ID, "reply", "blocked", "bound", "")
	operationOutput(t, f, s.ID, "file", "blocked", "", "")
	if _, err = f.g.discardOutput(context.Background(), f.room(), id, "owner"); !errors.Is(err, errOutputUnbound) {
		t.Fatal("unknown sibling dependency was guessed independent", err)
	}
}

func TestLatePreviewTargetAndDiscardClosureCommitTogether(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	file := operationOutput(t, f, s.ID, "file", "blocked", "same", "")
	if _, err = f.g.discardOutput(context.Background(), f.room(), file, "owner"); err != nil {
		t.Fatal(err)
	}
	reply := operationOutput(t, f, s.ID, "reply", "preparing", "same", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = f.g.acknowledgePreviewTarget(ctx, reply, f.room(), "acknowledged-late-card"); err != nil {
		t.Fatal("observed ACK was lost to concurrent render cancellation", err)
	}
	var target, state, status string
	if err = f.s.DB.QueryRow(`SELECT o.target,o.state,json_extract(d.detail,'$.closingStatus') FROM outbox o JOIN operator_decisions d ON d.id=o.id AND d.kind='output' WHERE o.id=?`, reply).Scan(&target, &state, &status); err != nil || target != "acknowledged-late-card" || state != "cancelled" || status == "" {
		t.Fatal("ACK target/closure not durable", target, state, status, err)
	}
}
