package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestFailedUsageRemainsVisibleAndIsRecoveredWithoutRepeatingAnswer(t *testing.T) {
	saved := finalPersistRetry
	finalPersistRetry = nil
	t.Cleanup(func() { finalPersistRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("first", "first task", "member"))
	w := f.worker()
	report := func(id string, d core.Delivery, input int64) core.WorkerEvent {
		ev := core.WorkerEvent{Type: "result", ResultID: id, UUID: d.UUID, UUIDs: []string{d.UUID}, Success: true, Text: "answer", Usage: core.Args{"input_tokens": input, "output_tokens": int64(1), "cache_read_input_tokens": int64(0), "cache_creation_input_tokens": int64(0)}}
		w.emit(ev)
		return ev
	}
	report("first-result", f.delivery(w), 10)
	if _, err := f.s.DB.Exec(`CREATE TRIGGER fail_usage_row BEFORE INSERT ON usage_results WHEN NEW.result_id='missed-result' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	f.send(f.message("missed", "second task", "member"))
	missed := report("missed-result", f.delivery(w), 50)
	eventually(t, func() bool { return f.status("missed") == "done" && f.tr.receiptDone("missed") })
	var observedAt int64
	if err := f.s.DB.QueryRow("SELECT at FROM usage_pending WHERE result_id='missed-result'").Scan(&observedAt); err != nil {
		t.Fatal("failed usage has no durable observation", err)
	}
	f.send(f.message("last", "third task", "member"))
	report("last-result", f.delivery(w), 20)
	before, err := f.g.usageReport(f.room())
	if err != nil {
		t.Fatal(err)
	}
	cumulative := argsOf(before["cumulative"])
	if before["pendingResults"] != int64(1) || before["historyIncomplete"] != true || argsOf(cumulative["usage"])["input_tokens"] != nil || argsOf(cumulative["reportedSubtotal"])["input_tokens"] != int64(30) {
		t.Fatalf("missing result was presented as a complete total: %s", jsonText(before))
	}
	var repliesBefore int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='reply'").Scan(&repliesBefore); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("DROP TRIGGER fail_usage_row"); err != nil {
		t.Fatal(err)
	}
	// Recovery uses the same persisted observation and never re-enters the
	// native result/answer path. A later duplicate remains a no-op too.
	f.g.retryUsage(context.Background())
	w.emit(missed)
	restartFixtureGateway(t, f)
	after, err := f.g.usageReport(f.room())
	if err != nil {
		t.Fatal(err)
	}
	cumulative = argsOf(after["cumulative"])
	if after["pendingResults"] != int64(0) || after["historyIncomplete"] != false || cumulative["results"] != int64(3) || argsOf(cumulative["usage"])["input_tokens"] != int64(80) {
		t.Fatalf("recovered usage lost or double counted a result: %s", jsonText(after))
	}
	var repliesAfter int
	var savedAt int64
	if err = f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='reply'").Scan(&repliesAfter); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow("SELECT at FROM usage_results WHERE result_id='missed-result'").Scan(&savedAt); err != nil {
		t.Fatal(err)
	}
	if repliesAfter != repliesBefore || f.starts.Load() != 1 || savedAt != observedAt {
		t.Fatalf("bookkeeping retry changed answers, work or observation time: replies=%d/%d starts=%d at=%d/%d", repliesBefore, repliesAfter, f.starts.Load(), observedAt, savedAt)
	}
}

func TestFinalAnswerCannotHideFailureOfBothUsageRecords(t *testing.T) {
	saved := finalPersistRetry
	finalPersistRetry = nil
	t.Cleanup(func() { finalPersistRetry = saved })
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	for _, query := range []string{
		`CREATE TRIGGER fail_usage_row BEFORE INSERT ON usage_results BEGIN SELECT RAISE(ABORT,'fixture'); END`,
		`CREATE TRIGGER fail_usage_pending BEFORE INSERT ON usage_pending BEGIN SELECT RAISE(ABORT,'fixture'); END`,
	} {
		if _, err := f.s.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	f.finish(w, d, "unpersisted answer")
	if f.status("first") != "uncertain" || f.tr.receiptDone("first") {
		t.Fatal("final answer claimed complete while both usage records were lost")
	}
	var finals int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='reply'").Scan(&finals); err != nil || finals != 0 {
		t.Fatalf("partial final commit: %d %v", finals, err)
	}
}

func TestUsageRetryCannotTurnTheFirstNativeResultIntoADuplicate(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("first", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	ev := core.WorkerEvent{Type: "result", ResultID: "first-native-result", UUID: d.UUID, Success: true, Usage: core.Args{"input_tokens": int64(50)}}
	// Pause at the real staging seam used by resultEvent: the ledger row is
	// committed, while the original result still owns its accounting phase.
	finish := f.g.stageResultUsage(s, ev)
	var once sync.Once
	release := func() { once.Do(finish) }
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	retried := make(chan struct{})
	go func() { f.g.retryUsage(ctx); close(retried) }()
	eventually(t, func() bool {
		f.g.usageLocks.mu.Lock()
		defer f.g.usageLocks.mu.Unlock()
		l := f.g.usageLocks.locks[d.SessionID]
		return l != nil && l.users == 2
	})
	select {
	case <-retried:
		t.Fatal("sweep processed a staged result before its native handler")
	default:
	}
	disposition := ""
	if err := f.s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		disposition, err = f.g.recordUsage(tx, s, ev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if disposition != "new" {
		t.Fatalf("the first answer would be discarded as %q", disposition)
	}
	release()
	select {
	case <-retried:
	case <-ctx.Done():
		t.Fatal("bookkeeping retry did not resume after native handling")
	}
	var results, pending int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM usage_results WHERE session=?", d.SessionID).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow("SELECT count(*) FROM usage_pending WHERE session=?", d.SessionID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if results != 1 || pending != 0 {
		t.Fatalf("bookkeeping duplicated or stranded the observation: results=%d pending=%d", results, pending)
	}
}

func TestMalformedUsageObservationsDoNotStarveLaterResults(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	ss, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		if _, err = f.s.DB.Exec("INSERT INTO usage_pending(session,result_id,room,at,event) VALUES(?,?,?,?,?)", ss.ID, fmt.Sprintf("bad-%d", i), r.Key, 1, "{broken"); err != nil {
			t.Fatal(err)
		}
	}
	s := &slot{room: r, session: ss}
	if err = f.s.Transaction(context.Background(), func(tx *sql.Tx) error {
		return f.g.stageUsage(tx, s, core.WorkerEvent{ResultID: "good", Success: true, Usage: core.Args{"input_tokens": int64(10)}})
	}); err != nil {
		t.Fatal(err)
	}
	f.g.retryUsage(context.Background())
	f.g.retryUsage(context.Background())
	var good, retained int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM usage_results WHERE result_id='good'").Scan(&good); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow("SELECT count(*) FROM usage_pending").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if good != 1 || retained != 64 || f.starts.Load() != 0 {
		t.Fatalf("retry starvation or discarded evidence: good=%d retained=%d starts=%d", good, retained, f.starts.Load())
	}
}
