package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestRecoveryTransitionAndWarningsCommitTogether(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	cc := core.NewID()
	if err = f.s.SetCC(s.ID, cc); err != nil {
		t.Fatal(err)
	}
	m := f.message("interrupted", "work", "member")
	d := core.Delivery{UUID: core.NewID(), RoomKey: r.Key, SessionID: s.ID, Message: m}
	if err = f.g.queueInput(d, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Mark(d.UUID, "sent"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("INSERT INTO requests(id,room,session,kind,digest,state,expires) VALUES('old-request',?,?,'permission','digest','pending',?)", r.Key, s.ID, time.Now().UnixMilli()+100000); err != nil {
		t.Fatal(err)
	}
	unknown, err := f.g.saveOutput(f.s.DB, r, s.ID, "unknown", true, "", "", "", nil, nil, "", "", "reply", "sending")
	if err != nil {
		t.Fatal(err)
	}
	known, err := f.g.saveOutput(f.s.DB, r, s.ID, "known", true, "known-card", "", "", nil, nil, "", "", "reply", "sending")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("CREATE TRIGGER recovery_write_failure BEFORE INSERT ON outbox WHEN NEW.kind='recovery' BEGIN SELECT RAISE(ABORT,'injected persistence failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err = f.g.Recover(context.Background()); err == nil {
		t.Fatal("recovery should fail atomically")
	}
	if f.status(m.ID) != "sent" {
		t.Fatal("interrupted evidence consumed without warning")
	}
	var state string
	if err = f.s.DB.QueryRow("SELECT state FROM requests WHERE id='old-request'").Scan(&state); err != nil || state != "pending" {
		t.Fatal("request transition was not rolled back", state, err)
	}
	if _, err = f.s.DB.Exec("DROP TRIGGER recovery_write_failure"); err != nil {
		t.Fatal(err)
	}
	if err = f.g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { o, e := f.g.output(known); return e == nil && o.State == "done" })
	if f.status(m.ID) != "uncertain" {
		t.Fatal("interrupted native input replayed")
	}
	o, err := f.g.output(unknown)
	if err != nil || o.State != "uncertain" || o.Target != "" {
		t.Fatal("unknown card replayed", o, err)
	}
	if err = f.s.DB.QueryRow("SELECT state FROM requests WHERE id='old-request'").Scan(&state); err != nil || state != "expired" {
		t.Fatal("old approval active", state, err)
	}
	var notices int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='recovery'").Scan(&notices); err != nil || notices != 1 {
		t.Fatal("missing durable recovery warning", notices, err)
	}
	if err = f.g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var again int
	_ = f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='recovery'").Scan(&again)
	if again != notices {
		t.Fatal("recovery duplicated warnings")
	}
	current, err := f.s.Session(r.Key)
	if err != nil || current.ID != s.ID || current.CCID != cc || f.starts.Load() != 0 {
		t.Fatal("recovery changed native identity or started a worker", current, err)
	}
}

func TestPendingPreviewDoesNotBlockDurableFinalOrShutdown(t *testing.T) {
	f := newFixture(t, nil)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.tr.cardHook = func(_ context.Context, _ core.Room, _ core.Args) (string, error) {
		close(entered)
		<-release
		defer close(finished)
		return "late-card", nil
	}
	f.send(f.message("one", "task", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "text", TurnID: d.UUID, Source: "foreground", BlockID: "block", Text: "preview", Partial: true})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("preview did not start")
	}
	f.finish(w, d, "complete final answer")
	var id string
	eventually(t, func() bool {
		return f.s.DB.QueryRow("SELECT id FROM outbox WHERE kind='reply' AND state='preparing'").Scan(&id) == nil
	})
	if f.status("one") != "done" {
		t.Fatal("final result must be persisted before the pending network response")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := f.g.Close(ctx); e != nil {
		t.Fatal(e)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("late request did not finish")
	}
	o, e := f.g.output(id)
	if e != nil || o.State != "uncertain" || o.Target != "" || !strings.Contains(o.Text, "complete final") {
		t.Fatal("late preview changed the durable recovery fence", o, e)
	}
	if f.tr.receiptDone("one") {
		t.Fatal("unconfirmed preview marked Done")
	}
}

func TestFileSendRequiresDurableSendingTransition(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, e := f.s.Session(r.Key)
	if e != nil {
		t.Fatal(e)
	}
	path := writeFixtureFile(t, "artifact.txt", "contents")
	id, e := f.g.saveOutput(f.s.DB, r, s.ID, "", true, "", "", "", nil, nil, "artifact.txt", path, "file", "")
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.s.DB.Exec("CREATE TRIGGER stop_before_file_handoff BEFORE UPDATE OF file_state ON outbox WHEN NEW.file_state='sending' BEGIN SELECT RAISE(IGNORE); END")
	if e != nil {
		t.Fatal(e)
	}
	ok, e := f.g.deliverOutput(context.Background(), id, r, false)
	if e != nil || ok {
		t.Fatal("ignored handoff should not deliver", ok, e)
	}
	f.tr.mu.Lock()
	calls := len(f.tr.files)
	f.tr.mu.Unlock()
	if calls != 0 {
		t.Fatal("provider called without durable file sending state")
	}
}
