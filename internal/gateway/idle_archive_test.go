package gateway

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
)

const testIdleWindow = int64(6 * 3600000)

func TestIdleChatSessionRotatesAfterThreshold(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Groups[0].IdleNewSessionMS = testIdleWindow })
	f.send(f.message("first", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, "first answer")
	eventually(t, func() bool { return f.status("first") == "done" })

	room := f.room()
	cur, found, err := f.s.CurrentSession(room.Key)
	if err != nil || !found {
		t.Fatalf("current session: %v %v", found, err)
	}
	if _, err = f.s.DB.Exec("UPDATE session_activity SET at=? WHERE session=?", time.Now().UnixMilli()-testIdleWindow-1000, cur.ID); err != nil {
		t.Fatal(err)
	}
	f.send(f.message("second", "again", "member"))
	w2 := f.worker()
	d2 := f.delivery(w2)
	if d2.SessionID == cur.ID {
		t.Fatal("idle chat input kept the old session")
	}
	if !strings.Contains(d2.Notice, "已开新会话") || !strings.Contains(d2.Notice, "/resume") {
		t.Fatalf("delivery notice = %q", d2.Notice)
	}
	f.finish(w2, d2, "second answer")
	eventually(t, func() bool { return f.status("second") == "done" })

	var oldState string
	if err = f.s.DB.QueryRow("SELECT state FROM sessions WHERE id=?", cur.ID).Scan(&oldState); err != nil || oldState != "archived" {
		t.Fatalf("old session state = %q, %v", oldState, err)
	}
	fresh, found, err := f.s.CurrentSession(room.Key)
	if err != nil || !found || fresh.ID != d2.SessionID {
		t.Fatalf("current session after rotation: %+v %v %v", fresh, found, err)
	}
	var audits int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='idle_new_session'", room.Key).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("idle rotation audits = %d, %v", audits, err)
	}
}

func TestIdleNoticeRendersOnReplyCard(t *testing.T) {
	f := newFixture(t, nil)
	notice := idleNewSessionNotice(testIdleWindow)
	anchor := &core.Delivery{Message: core.Message{ID: "om", SenderID: "member", Time: time.Now().UnixMilli()}, Notice: notice}
	render := newRender(f.g, &slot{room: f.room(), session: core.Session{ID: "session"}, ctx: context.Background()}, anchor)
	if render.notice != notice || !strings.Contains(render.notice, "6 小时") {
		t.Fatalf("render notice = %q", render.notice)
	}
}

func TestIdleRotationSkipsThreadScopeAndAdoptedTopics(t *testing.T) {
	t.Run("thread scope", func(t *testing.T) {
		f := newFixture(t, func(c *core.Config) {
			c.Groups[0].SessionScope = core.SessionScopeThread
			c.Groups[0].IdleNewSessionMS = testIdleWindow
		})
		f.send(f.message("t1", "start", "member"))
		w := f.worker()
		d := f.delivery(w)
		f.finish(w, d, "answer")
		eventually(t, func() bool { return f.status("t1") == "done" })
		room := f.topic("t1")
		cur := f.openSession(room.Key)
		if _, err := f.s.DB.Exec("UPDATE session_activity SET at=? WHERE session=?", time.Now().UnixMilli()-testIdleWindow-1000, cur.ID); err != nil {
			t.Fatal(err)
		}
		f.send(inTopic(f.message("t1b", "continue", "member"), "t1", "omt_1"))
		d2 := f.delivery(w)
		if d2.SessionID != cur.ID || d2.Notice != "" {
			t.Fatalf("thread topic rotated: %+v", d2)
		}
	})

	t.Run("adopted topic", func(t *testing.T) {
		f, work, native := adoptFixtureWith(t, func(c *core.Config) { c.DMs[0].IdleNewSessionMS = testIdleWindow })
		pid := os.Getpid()
		started, ok := process.StartTime(pid)
		if !ok {
			t.Skip("process start time is unavailable")
		}
		req := attachRequest(native, work)
		req.HolderPID, req.HolderStarted = pid, started
		result := f.g.Attach(context.Background(), req)
		if !result.OK {
			t.Fatalf("attach: %+v", result)
		}
		room, ok := f.g.roomFromKey(result.Room)
		if !ok {
			t.Fatal("adopted room did not resolve")
		}
		cur, found, err := f.s.CurrentSession(room.Key)
		if err != nil || !found {
			t.Fatalf("current session: %v %v", found, err)
		}
		if _, err = f.s.DB.Exec("UPDATE session_activity SET at=? WHERE session=?", time.Now().UnixMilli()-testIdleWindow-1000, cur.ID); err != nil {
			t.Fatal(err)
		}
		f.send(core.Message{ID: "om_idle", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: room.Thread, Text: "hello", Time: time.Now().UnixMilli(), Resources: []core.Resource{}})
		eventually(t, func() bool { return f.status("om_idle") == "queued" })
		fresh, found, err := f.s.CurrentSession(room.Key)
		if err != nil || !found || fresh.ID != cur.ID {
			t.Fatalf("adopted topic rotated: %+v %v %v", fresh, found, err)
		}
		var audits int
		if err = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='idle_new_session'", room.Key).Scan(&audits); err != nil || audits != 0 {
			t.Fatalf("adopted topic rotation audits = %d, %v", audits, err)
		}
	})
}

func TestArchivedTopicMessageRestoresInPlace(t *testing.T) {
	f, work, native := adoptFixture(t)
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found {
		t.Fatalf("record: %v %v", found, err)
	}
	if _, err = f.s.DB.Exec("UPDATE adopted_topics SET last_active=? WHERE native=?", time.Now().UnixMilli()-8*86400000, native); err != nil {
		t.Fatal(err)
	}
	if archived, err := f.s.ArchiveAdoption(native, time.Now().UnixMilli()); err != nil || !archived {
		t.Fatalf("archive: %v %v", archived, err)
	}
	room, ok := f.g.roomFromKey(record.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	f.send(core.Message{ID: "om_restore", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: room.Thread, Text: "hello again", Time: time.Now().UnixMilli(), Resources: []core.Resource{}})
	w := f.worker()
	d := f.delivery(w)
	if d.SessionID != record.Session {
		t.Fatalf("restored topic did not continue its session: %+v", d)
	}
	fresh, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found || fresh.State != store.AdoptedOpen {
		t.Fatalf("restored state: %+v %v %v", fresh, found, err)
	}
}

func TestNotifyRestoresArchivedTopic(t *testing.T) {
	f, work, native := adoptFixture(t)
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	if archived, err := f.s.ArchiveAdoption(native, time.Now().UnixMilli()); err != nil || !archived {
		t.Fatalf("archive: %v %v", archived, err)
	}
	notified := f.g.Notify(context.Background(), notifyRequest(native, work, "after archive"))
	if !notified.OK || notified.Created || notified.State != store.AdoptedOpen || notified.MessageID == "" {
		t.Fatalf("notify after archive: %+v", notified)
	}
	fresh, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found || fresh.State != store.AdoptedOpen {
		t.Fatalf("restored state: %+v %v %v", fresh, found, err)
	}
}

func TestSweepArchivesInactiveAdoptionAndRemovesEndpoint(t *testing.T) {
	f, work, native := adoptFixtureWith(t, func(c *core.Config) { c.AdoptedArchiveMS = 1000 })
	pid := os.Getpid()
	started, ok := process.StartTime(pid)
	if !ok {
		t.Skip("process start time is unavailable")
	}
	req := attachRequest(native, work)
	req.HolderPID, req.HolderStarted = pid, started
	req.MessagingSocket = `\\.\pipe\archive-inbox`
	req.MessagingToken = "archive-token"
	result := f.g.Attach(context.Background(), req)
	if !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found {
		t.Fatalf("record: %v %v", found, err)
	}
	path, ok := f.g.adoptEndpointPath(native)
	if !ok {
		t.Fatal("native ID has no endpoint path")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("endpoint file: %v", err)
	}
	// The terminal exited after registering the endpoint; the stale file is
	// removed only when the inactive topic is archived.
	if _, err = f.s.ClearAdoptionHolder(native, pid, started); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err = f.s.DB.Exec("UPDATE adopted_topics SET last_active=? WHERE native=?", now-1000, native); err != nil {
		t.Fatal(err)
	}
	f.g.Sweep(context.Background(), now)
	fresh, _, _ := f.s.AdoptedTopicByNative(native)
	if fresh.State != store.AdoptedOpen {
		t.Fatalf("boundary activity archived the topic: %s", fresh.State)
	}
	if _, err = f.s.DB.Exec("UPDATE adopted_topics SET last_active=? WHERE native=?", now-1001, native); err != nil {
		t.Fatal(err)
	}
	f.g.Sweep(context.Background(), now)
	fresh, _, _ = f.s.AdoptedTopicByNative(native)
	if fresh.State != store.AdoptedArchived {
		t.Fatalf("inactive topic state = %q", fresh.State)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("archived endpoint file remains: %v", err)
	}
	if _, ok := f.g.endpointFor(record.Room); ok {
		t.Fatal("archived endpoint remains in memory")
	}
}
