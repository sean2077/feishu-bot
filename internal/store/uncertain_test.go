package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestUncertainSummaryEmpty(t *testing.T) {
	s := openTest(t)
	summary, err := s.UncertainSummary(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, bucket := range map[string]UncertainBucket{"inbox": summary.Inbox, "outbox": summary.Outbox} {
		if bucket.Count != 0 || bucket.OldestAt != nil || bucket.RoomCount != 0 || len(bucket.Rooms) != 0 || len(bucket.Samples) != 0 {
			t.Fatalf("%s summary on empty store = %#v", name, bucket)
		}
	}
}

func TestUncertainSummaryCountsOnlyStuckStatesAndDeduplicatesRooms(t *testing.T) {
	s := openTest(t)
	insert := func(query string, args ...any) {
		t.Helper()
		if _, err := s.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert(`INSERT INTO inbox(room,id,message,status,at) VALUES('room-a','om_old_uncertain','{}','uncertain',100)`)
	insert(`INSERT INTO inbox(room,id,message,status,at) VALUES('room-b','om_new','{}','uncertain',200)`)
	insert(`INSERT INTO inbox(room,id,message,status,at) VALUES('room-a','om_mid','{}','uncertain',150)`)
	insert(`INSERT INTO inbox(room,id,message,status,at) VALUES('room-a','om_done','{}','done',50)`)
	insert(`INSERT INTO inbox(room,id,message,status,at) VALUES('room-a','om_queued','{}','queued',10)`)
	insert(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-blocked','room-b','s','',0,'blocked',300)`)
	insert(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-uncertain','room-a','s','',0,'uncertain',250)`)
	insert(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-pending','room-a','s','',0,'pending',100)`)
	insert(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-done','room-a','s','',0,'done',10)`)
	insert(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-ack','room-a','s','',0,'acknowledged',10)`)

	summary, err := s.UncertainSummary(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox := summary.Inbox
	if inbox.Count != 3 || inbox.OldestAt == nil || *inbox.OldestAt != 100 || inbox.RoomCount != 2 {
		t.Fatalf("inbox summary = %#v", inbox)
	}
	if strings.Join(inbox.Rooms, ",") != "room-a,room-b" {
		t.Fatalf("inbox rooms = %#v", inbox.Rooms)
	}
	if len(inbox.Samples) != 3 || !strings.HasPrefix(inbox.Samples[0], "om_old_u") || !strings.HasSuffix(inbox.Samples[0], "…") {
		t.Fatalf("inbox samples = %#v", inbox.Samples)
	}
	outbox := summary.Outbox
	if outbox.Count != 2 || outbox.OldestAt == nil || *outbox.OldestAt != 250 || outbox.RoomCount != 2 {
		t.Fatalf("outbox summary = %#v", outbox)
	}
	if strings.Join(outbox.Samples, ",") != "out-unce…,out-bloc…" {
		t.Fatalf("outbox samples = %#v", outbox.Samples)
	}

	scoped, err := s.UncertainSummary(context.Background(), "room-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Inbox.Count != 2 || scoped.Outbox.Count != 1 || len(scoped.Inbox.Rooms) != 1 || scoped.Inbox.Rooms[0] != "room-a" {
		t.Fatalf("room-scoped summary = %#v", scoped)
	}

	before := int64(400)
	cutoff, err := s.UncertainSummary(context.Background(), "", &before)
	if err != nil {
		t.Fatal(err)
	}
	if cutoff.Inbox.Count != 3 || cutoff.Outbox.Count != 2 {
		t.Fatalf("cutoff summary = %#v", cutoff)
	}
	before = 150
	cutoff, err = s.UncertainSummary(context.Background(), "", &before)
	if err != nil {
		t.Fatal(err)
	}
	if cutoff.Inbox.Count != 1 || cutoff.Inbox.OldestAt == nil || *cutoff.Inbox.OldestAt != 100 || cutoff.Outbox.Count != 0 {
		t.Fatalf("older-only cutoff summary = %#v", cutoff)
	}
}

func TestUncertainSummaryBoundsRoomsAndSamples(t *testing.T) {
	s := openTest(t)
	for i := 0; i < 25; i++ {
		if _, err := s.DB.Exec(`INSERT INTO inbox(room,id,message,status,at) VALUES(?,?, '{}','uncertain',?)`, fmt.Sprintf("room-%02d", i), fmt.Sprintf("om_%02d", i), i+1); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := s.UncertainSummary(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Inbox.RoomCount != 25 || len(summary.Inbox.Rooms) != uncertainRoomLimit {
		t.Fatalf("room bound = count %d listed %d", summary.Inbox.RoomCount, len(summary.Inbox.Rooms))
	}
	if len(summary.Inbox.Samples) != uncertainSampleLimit {
		t.Fatalf("sample bound = %#v", summary.Inbox.Samples)
	}
	for _, room := range summary.Inbox.Rooms {
		if !strings.HasPrefix(room, "room-") {
			t.Fatalf("unexpected room key %q", room)
		}
	}
}

func TestAckUncertainOnlyMarksEligibleRecords(t *testing.T) {
	s := openTest(t)
	for _, query := range []string{
		`INSERT INTO inbox(room,id,message,status,at) VALUES('room','om_old','{}','uncertain',100)`,
		`INSERT INTO inbox(room,id,message,status,at) VALUES('room','om_new','{}','uncertain',300)`,
		`INSERT INTO inbox(room,id,message,status,at) VALUES('room','om_done','{}','done',100)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-uncertain','room','s','',0,'uncertain',100)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-blocked','room','s','',0,'blocked',100)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-new','room','s','',0,'uncertain',300)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-pending','room','s','',0,'pending',100)`,
	} {
		if _, err := s.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.AckUncertain(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	if result.Inbox != 1 || result.Outbox != 2 {
		t.Fatalf("ack result = %#v", result)
	}
	for query, want := range map[string]string{
		`SELECT status FROM inbox WHERE id='om_old'`:        "acknowledged",
		`SELECT status FROM inbox WHERE id='om_new'`:        "uncertain",
		`SELECT status FROM inbox WHERE id='om_done'`:       "done",
		`SELECT state FROM outbox WHERE id='out-uncertain'`: "acknowledged",
		`SELECT state FROM outbox WHERE id='out-blocked'`:   "acknowledged",
		`SELECT state FROM outbox WHERE id='out-new'`:       "uncertain",
		`SELECT state FROM outbox WHERE id='out-pending'`:   "pending",
	} {
		var got string
		if err := s.DB.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %q (%v), want %q", query, got, err, want)
		}
	}
	again, err := s.AckUncertain(context.Background(), 200)
	if err != nil || again.Inbox != 0 || again.Outbox != 0 {
		t.Fatalf("second ack = %#v %v", again, err)
	}
	// Acknowledged input does not remain pending for admission or handoff.
	var pending int
	if err := s.DB.QueryRow(`SELECT count(*) FROM inbox WHERE status IN ('queued','sent','consumed','uncertain')`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending inputs after ack = %d %v", pending, err)
	}
}

func TestMaintenanceTreatsAcknowledgedOutboxAsTerminal(t *testing.T) {
	s := openTest(t)
	if _, err := s.DB.Exec(`INSERT INTO outputs(id,room,session,at) VALUES('old-output','room','session',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO outbox(id,room,session,text,target,success,state,at) VALUES('ack','room','session','', 'old-output',0,'acknowledged',1)`); err != nil {
		t.Fatal(err)
	}
	p := maintenancePolicyNamed(t, "outputs", 40*dayMS)
	if _, err := s.maintainPolicy(context.Background(), p, 0, false); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT count(*) FROM outputs WHERE id='old-output'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("acknowledged outbox blocked terminal output retention: %d %v", n, err)
	}
}
