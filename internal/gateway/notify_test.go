package gateway

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	nativepkg "github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
)

func notifyRequest(native, work, summary string) core.NotifyRequest {
	return core.NotifyRequest{
		Kind: core.ControlKindNotify, ID: core.NewID(), At: time.Now().UnixMilli(),
		User: "member", Native: native, CWD: work, Summary: summary,
		ConfigPath: "C:/config/config.json", MessagingSocket: "endpoint", MessagingToken: "token",
	}
}

func TestNotifyCreatesThenAppends(t *testing.T) {
	f, work, native := adoptFixture(t)
	first := f.g.Notify(context.Background(), notifyRequest(native, work, "first result"))
	if !first.OK || !first.Created || first.State != store.AdoptedOpen || first.MessageID == "" || first.RootMessageID == "" {
		t.Fatalf("first notify: %+v", first)
	}
	if len(f.tr.userCards) != 1 {
		t.Fatalf("root cards = %d", len(f.tr.userCards))
	}
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found || record.State != store.AdoptedOpen || record.Root != first.RootMessageID || record.Thread != first.ThreadID {
		t.Fatalf("record: %+v %v %v", record, found, err)
	}
	second := f.g.Notify(context.Background(), notifyRequest(native, work, "second result"))
	if !second.OK || second.Created || second.RootMessageID != first.RootMessageID || second.ThreadID != first.ThreadID {
		t.Fatalf("second notify: %+v", second)
	}
	if second.MessageID == "" || second.MessageID == first.MessageID {
		t.Fatalf("append message id: %q vs %q", second.MessageID, first.MessageID)
	}
	if len(f.tr.userCards) != 1 {
		t.Fatalf("append sent another root card: %d", len(f.tr.userCards))
	}
	f.tr.mu.Lock()
	notices := strings.Join(f.tr.notices, "\n")
	f.tr.mu.Unlock()
	if !strings.Contains(notices, "second result") {
		t.Fatalf("append text missing: %s", notices)
	}
}

func TestNotifyBusyRoomKeepsGatewayHolder(t *testing.T) {
	f, work, native := adoptFixture(t)
	if result := f.g.Attach(context.Background(), attachRequest(native, work)); !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	room, ok := f.g.roomFromKey(record.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	slot, err := f.g.reserve(room)
	if err != nil || slot == nil {
		t.Fatalf("reserve: %+v %v", slot, err)
	}
	defer func() {
		slot.cancel()
		f.g.mu.Lock()
		delete(f.g.slots, room.Key)
		f.g.mu.Unlock()
	}()
	req := notifyRequest(native, work, "busy notification")
	req.HolderPID = os.Getpid()
	if started, ok := process.StartTime(req.HolderPID); ok {
		req.HolderStarted = started
	}
	req.MessagingSocket = `\\.\pipe\fixture-inbox`
	req.MessagingToken = "secret-token-value"
	result := f.g.Notify(context.Background(), req)
	if !result.OK || !strings.Contains(strings.Join(result.Warnings, "\n"), "未登记本地转递") {
		t.Fatalf("busy notify: %+v", result)
	}
	record, _, _ = f.s.AdoptedTopicByNative(native)
	if record.HolderPID != 0 {
		t.Fatalf("busy notify took the holder: %d", record.HolderPID)
	}
	if _, ok := f.g.endpointFor(record.Room); ok {
		t.Fatal("busy notify registered an endpoint")
	}
}

func TestNotifyIdleRoomTakesHolder(t *testing.T) {
	f, work, native := adoptFixture(t)
	if result := f.g.Attach(context.Background(), attachRequest(native, work)); !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	req := notifyRequest(native, work, "idle notification")
	req.HolderPID = os.Getpid()
	if started, ok := process.StartTime(req.HolderPID); ok {
		req.HolderStarted = started
	}
	req.MessagingSocket = `\\.\pipe\fixture-inbox`
	req.MessagingToken = "secret-token-value"
	result := f.g.Notify(context.Background(), req)
	if !result.OK {
		t.Fatalf("idle notify: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if record.HolderPID != req.HolderPID || record.HolderStarted != req.HolderStarted {
		t.Fatalf("holder = %d/%d, want %d/%d", record.HolderPID, record.HolderStarted, req.HolderPID, req.HolderStarted)
	}
	if _, ok := f.g.endpointFor(record.Room); !ok {
		t.Fatal("idle notify did not register the endpoint")
	}
}

func TestNotifyHolderWriteFailureKeepsNotification(t *testing.T) {
	f, work, native := adoptFixture(t)
	if result := f.g.Attach(context.Background(), attachRequest(native, work)); !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	if _, err := f.s.DB.Exec(`CREATE TRIGGER fixture_holder_failure BEFORE UPDATE OF holder_pid ON adopted_topics BEGIN SELECT RAISE(ABORT,'fixture holder failure'); END`); err != nil {
		t.Fatal(err)
	}
	req := notifyRequest(native, work, "holder failure notification")
	req.HolderPID = os.Getpid()
	req.MessagingSocket = `\\.\pipe\fixture-inbox`
	req.MessagingToken = "secret-token-value"
	result := f.g.Notify(context.Background(), req)
	if !result.OK || !strings.Contains(strings.Join(result.Warnings, "\n"), "本地持有者写入失败") {
		t.Fatalf("holder failure notify: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if record.HolderPID != 0 {
		t.Fatalf("holder write failure changed holder: %d", record.HolderPID)
	}
	if _, ok := f.g.endpointFor(record.Room); ok {
		t.Fatal("holder write failure registered an endpoint")
	}
}

func TestNotifyPendingAndUncertainDoNotResend(t *testing.T) {
	f, work, native := adoptFixture(t)
	if _, _, err := f.s.BeginAdoption(native, "member", work, 0, 0); err != nil {
		t.Fatal(err)
	}
	pending := f.g.Notify(context.Background(), notifyRequest(native, work, "pending"))
	if pending.OK || pending.State != store.AdoptedPending || pending.Created || len(f.tr.userCards) != 0 {
		t.Fatalf("pending notify: %+v cards=%d", pending, len(f.tr.userCards))
	}
	if err := f.s.MarkAdoptionUncertain(native); err != nil {
		t.Fatal(err)
	}
	uncertain := f.g.Notify(context.Background(), notifyRequest(native, work, "uncertain"))
	if uncertain.OK || uncertain.State != store.AdoptedUncertain || uncertain.Created || len(f.tr.userCards) != 0 {
		t.Fatalf("uncertain notify: %+v cards=%d", uncertain, len(f.tr.userCards))
	}
}

func TestNotifyNotConfigured(t *testing.T) {
	f := newFixture(t, nil)
	result := f.g.Notify(context.Background(), core.NotifyRequest{
		Kind: core.ControlKindNotify, ID: core.NewID(), At: time.Now().UnixMilli(),
		User: "member", Native: "native", Summary: "result",
	})
	if result.OK || result.Error == nil || result.Error.Code != core.NotifyErrorNotConfigured {
		t.Fatalf("not configured result: %+v", result)
	}
	if len(f.tr.userCards) != 0 {
		t.Fatal("not configured notify sent a card")
	}
}

func relayFixture(t *testing.T) (*fixture, string, string, core.AttachResult) {
	t.Helper()
	f, work, native := adoptFixture(t)
	req := attachRequest(native, work)
	req.SessionName = "fixture-session"
	req.HolderPID = os.Getpid()
	if started, ok := process.StartTime(req.HolderPID); ok {
		req.HolderStarted = started
	}
	req.MessagingSocket = `\\.\pipe\fixture-inbox`
	req.MessagingToken = "secret-token-value"
	req.ConfigPath = "C:/config/config.json"
	result := f.g.Attach(context.Background(), req)
	if !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	return f, work, native, result
}

func relayMessage(f *fixture, id, text, root string) core.Message {
	return core.Message{ID: id, ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: root, Text: text, Time: time.Now().UnixMilli(), Resources: []core.Resource{}}
}

func TestTopicRelayAndReply(t *testing.T) {
	f, _, native, result := relayFixture(t)
	var sent []string
	f.g.adoptSender = func(_ context.Context, socket, token, text string) (nativepkg.InboxOutcome, error) {
		if socket != `\\.\pipe\fixture-inbox` || token != "secret-token-value" {
			t.Fatalf("endpoint = %q %q", socket, token)
		}
		sent = append(sent, text)
		return nativepkg.InboxSubmitted, nil
	}
	msg := relayMessage(f, "om_relay_1", "hello from Feishu", result.RootMessageID)
	f.send(msg)
	if len(sent) != 1 || !strings.Contains(sent[0], "网关已校验为 owner 本人") || !strings.Contains(sent[0], "om_relay_1") || !strings.Contains(sent[0], "topic reply") {
		t.Fatalf("wrapper: %v", sent)
	}
	if !strings.Contains(sent[0], "feishu-bot topic reply --message-id om_relay_1 --text-file -") {
		t.Fatalf("wrapper command: %v", sent)
	}
	if strings.Contains(sent[0], "--native") || strings.Contains(sent[0], "--config") {
		t.Fatalf("wrapper asked for redundant flags: %v", sent)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if _, status, found, err := f.s.Forwarded(record.Room, "om_relay_1"); err != nil || !found || status != store.InputForwarded {
		t.Fatalf("forwarded row: %q %v %v", status, found, err)
	}
	waitFor(t, "relay notice", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, "已提交给本机会话") {
				return true
			}
		}
		return false
	})
	reply := f.g.TopicReply(context.Background(), core.TopicReplyRequest{Kind: core.ControlKindReply, ID: core.NewID(), At: time.Now().UnixMilli(), Native: native, MessageID: "om_relay_1", Text: "answer from terminal"})
	if !reply.OK || reply.MessageID == "" {
		t.Fatalf("topic reply: %+v", reply)
	}
	if _, status, _, _ := f.s.Forwarded(record.Room, "om_relay_1"); status != "done" {
		t.Fatalf("forwarded status = %q", status)
	}
	f.tr.mu.Lock()
	notices := strings.Join(f.tr.notices, "\n")
	f.tr.mu.Unlock()
	if !strings.Contains(notices, "answer from terminal") {
		t.Fatalf("reply not delivered: %s", notices)
	}
	again := f.g.TopicReply(context.Background(), core.TopicReplyRequest{Kind: core.ControlKindReply, ID: core.NewID(), At: time.Now().UnixMilli(), Native: native, MessageID: "om_relay_1", Text: "duplicate"})
	if again.OK || again.Error == nil || again.Error.Code != core.NotifyErrorInvalidRequest || !strings.Contains(again.Error.Message, "已回复") {
		t.Fatalf("duplicate reply: %+v", again)
	}
}

func TestTopicReplyRejectsInvalidNative(t *testing.T) {
	f, _, _, _ := relayFixture(t)
	reply := f.g.TopicReply(context.Background(), core.TopicReplyRequest{Kind: core.ControlKindReply, ID: core.NewID(), At: time.Now().UnixMilli(), Native: "[not-a-session]", MessageID: "om_x", Text: "answer"})
	if reply.OK || reply.Error == nil || reply.Error.Code != core.NotifyErrorInvalidRequest || reply.Error.Message != "--native 不是合法会话 ID" {
		t.Fatalf("invalid native reply: %+v", reply)
	}
}

func TestTopicReplyReportsOwningSession(t *testing.T) {
	f, work, native, result := relayFixture(t)
	f.g.adoptSender = func(context.Context, string, string, string) (nativepkg.InboxOutcome, error) {
		return nativepkg.InboxSubmitted, nil
	}
	f.send(relayMessage(f, "om_other_owner", "hello", result.RootMessageID))
	other := "22222222-3333-4444-5555-666666666666"
	room, ok := f.c.AdoptedTopicRoom("oc_adopted", "member", "om_other_root")
	if !ok {
		t.Fatal("second adopted room did not resolve")
	}
	if _, _, err := f.s.BeginAdoption(other, "member", work, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BindAdoptionRoot(other, room.Key, "om_other_root", "session-other", "/config", false, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.OpenAdoption(other, "omt_other"); err != nil {
		t.Fatal(err)
	}
	reply := f.g.TopicReply(context.Background(), core.TopicReplyRequest{Kind: core.ControlKindReply, ID: core.NewID(), At: time.Now().UnixMilli(), Native: other, MessageID: "om_other_owner", Text: "answer"})
	want := "消息 om_other_owner 属于会话 " + core.ShortNative(native) + "，传入的是 " + core.ShortNative(other)
	if reply.OK || reply.Error == nil || reply.Error.Code != core.NotifyErrorInvalidRequest || reply.Error.Message != want {
		t.Fatalf("owner mismatch reply: %+v, want %q", reply, want)
	}
}

func TestTopicReplyReportsClosedTopic(t *testing.T) {
	f, _, native, result := relayFixture(t)
	f.g.adoptSender = func(context.Context, string, string, string) (nativepkg.InboxOutcome, error) {
		return nativepkg.InboxSubmitted, nil
	}
	f.send(relayMessage(f, "om_closed_topic", "hello", result.RootMessageID))
	if _, err := f.s.DB.Exec("UPDATE adopted_topics SET state='uncertain' WHERE native=?", native); err != nil {
		t.Fatal(err)
	}
	reply := f.g.TopicReply(context.Background(), core.TopicReplyRequest{Kind: core.ControlKindReply, ID: core.NewID(), At: time.Now().UnixMilli(), Native: native, MessageID: "om_closed_topic", Text: "answer"})
	if reply.OK || reply.Error == nil || reply.Error.Code != core.NotifyErrorInvalidRequest || reply.Error.Message != "没有处于 open 状态的认领话题" {
		t.Fatalf("closed topic reply: %+v", reply)
	}
}

func TestTopicRelayUncertainDoesNotQueue(t *testing.T) {
	f, _, native, result := relayFixture(t)
	f.g.adoptSender = func(context.Context, string, string, string) (nativepkg.InboxOutcome, error) {
		return nativepkg.InboxUncertain, os.ErrDeadlineExceeded
	}
	f.send(relayMessage(f, "om_relay_uncertain", "hello", result.RootMessageID))
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if _, status, found, err := f.s.Forwarded(record.Room, "om_relay_uncertain"); err != nil || !found || status != "uncertain" {
		t.Fatalf("forwarded row: %q %v %v", status, found, err)
	}
	var status string
	if err := f.s.DB.QueryRow("SELECT status FROM inbox WHERE room=? AND id=?", record.Room, "om_relay_uncertain").Scan(&status); err != nil || status != "uncertain" {
		t.Fatalf("inbox status: %q %v", status, err)
	}
	if f.starts.Load() != 0 {
		t.Fatal("uncertain relay started a worker or queued input")
	}
	waitFor(t, "uncertain notice", func() bool { return noticeContains(f, "不会自动重放") })
}

func TestTopicRelayFallsBackWhenInboxFails(t *testing.T) {
	f, _, native, result := relayFixture(t)
	f.g.adoptSender = func(context.Context, string, string, string) (nativepkg.InboxOutcome, error) {
		return nativepkg.InboxNotSent, os.ErrClosed
	}
	msg := relayMessage(f, "om_relay_fail", "hello", result.RootMessageID)
	f.send(msg)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if _, status, found, _ := f.s.Forwarded(record.Room, "om_relay_fail"); found && status == store.InputForwarded {
		t.Fatalf("failed submission stayed forwarded: %q", status)
	}
	var status string
	if err := f.s.DB.QueryRow("SELECT status FROM inbox WHERE room=? AND id=?", record.Room, "om_relay_fail").Scan(&status); err != nil || status != "queued" {
		t.Fatalf("fallback status = %q %v", status, err)
	}
	if f.starts.Load() != 0 {
		t.Fatal("failed relay started a worker")
	}
	waitFor(t, "relay fallback notice", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, "转递失败") {
				return true
			}
		}
		return false
	})
}

func TestTopicRelayUnansweredBecomesUncertainOnExit(t *testing.T) {
	f, _, native, result := relayFixture(t)
	f.g.adoptSender = func(context.Context, string, string, string) (nativepkg.InboxOutcome, error) {
		return nativepkg.InboxSubmitted, nil
	}
	msg := relayMessage(f, "om_relay_pending", "hello", result.RootMessageID)
	f.send(msg)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if _, status, _, _ := f.s.Forwarded(record.Room, "om_relay_pending"); status != store.InputForwarded {
		t.Fatalf("forwarded status = %q", status)
	}
	if err := f.s.UpdateAdoptionHolder(native, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	if _, status, _, _ := f.s.Forwarded(record.Room, "om_relay_pending"); status != "uncertain" {
		t.Fatalf("exit status = %q", status)
	}
	waitFor(t, "forward failure notice", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, "不会重放") {
				return true
			}
		}
		return false
	})
}

func TestTopicRelayIgnoresNonOwner(t *testing.T) {
	f, _, _, result := relayFixture(t)
	called := false
	f.g.adoptSender = func(context.Context, string, string, string) (nativepkg.InboxOutcome, error) {
		called = true
		return nativepkg.InboxSubmitted, nil
	}
	msg := relayMessage(f, "om_other", "hello", result.RootMessageID)
	msg.SenderID = "stranger"
	f.send(msg)
	if called {
		t.Fatal("non-owner message was forwarded")
	}
}

func TestTopicRelayTokenNeverAudited(t *testing.T) {
	f, _, native, result := relayFixture(t)
	var logs []string
	f.g.logger = func(event string, detail core.Args) {
		data, _ := json.Marshal(detail)
		logs = append(logs, event+" "+string(data))
	}
	f.g.adoptSender = func(context.Context, string, string, string) (nativepkg.InboxOutcome, error) {
		return nativepkg.InboxSubmitted, nil
	}
	msg := relayMessage(f, "om_relay_token", "hello", result.RootMessageID)
	f.send(msg)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if _, _, _, err := f.s.Forwarded(record.Room, "om_relay_token"); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.DB.Query("SELECT event,detail FROM audit")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var event, detail string
		if rows.Scan(&event, &detail) == nil {
			logs = append(logs, event+" "+detail)
		}
	}
	for _, entry := range logs {
		if strings.Contains(entry, "secret-token-value") {
			t.Fatalf("token leaked into audit/log: %s", entry)
		}
	}
}

func TestNotifyRejectsDifferentLiveHolder(t *testing.T) {
	f, work, native := adoptFixture(t)
	pid := os.Getpid()
	started, ok := process.StartTime(pid)
	if !ok {
		t.Skip("process start time is unavailable")
	}
	req := attachRequest(native, work)
	req.HolderPID, req.HolderStarted = pid, started
	if result := f.g.Attach(context.Background(), req); !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	parent := os.Getppid()
	if parent <= 0 || parent == pid {
		t.Skip("no distinct live parent process")
	}
	notify := notifyRequest(native, work, "held")
	notify.HolderPID = parent
	notify.HolderStarted = 0
	result := f.g.Notify(context.Background(), notify)
	if result.OK || result.Error == nil || result.Error.Code != core.NotifyErrorInvalidRequest {
		t.Fatalf("different holder notify: %+v", result)
	}
}

func TestTopicRelayPreservesOrder(t *testing.T) {
	f, _, _, result := relayFixture(t)
	var sent []string
	f.g.adoptSender = func(_ context.Context, _, _, text string) (nativepkg.InboxOutcome, error) {
		sent = append(sent, text)
		return nativepkg.InboxSubmitted, nil
	}
	f.send(relayMessage(f, "om_order_1", "first", result.RootMessageID))
	f.send(relayMessage(f, "om_order_2", "second", result.RootMessageID))
	if len(sent) != 2 || !strings.Contains(sent[0], "first") || !strings.Contains(sent[1], "second") {
		t.Fatalf("order: %v", sent)
	}
}

func TestTopicReplyRejectsUnknownMessage(t *testing.T) {
	f, _, native, _ := relayFixture(t)
	reply := f.g.TopicReply(context.Background(), core.TopicReplyRequest{Kind: core.ControlKindReply, ID: core.NewID(), At: time.Now().UnixMilli(), Native: native, MessageID: "om_missing", Text: "answer"})
	if reply.OK || reply.Error == nil || reply.Error.Code != core.NotifyErrorInvalidRequest {
		t.Fatalf("unknown reply: %+v", reply)
	}
}
