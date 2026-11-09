package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func forwardedApprovalRoom(t *testing.T, f *fixture) (core.Room, string) {
	t.Helper()
	r, ok := RoomFor(f.c, "dm-member", "member")
	if !ok {
		t.Fatal("fixture DM unavailable")
	}
	ss, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	return r, ss.ID
}

func hasApprovalNotice(f *fixture, fragment string) bool {
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	for _, notice := range f.tr.notices {
		if strings.Contains(notice, fragment) {
			return true
		}
	}
	return false
}

func TestForwardedApprovalReportsDeliveryOnlyAfterCardAck(t *testing.T) {
	f := newFixture(t, nil)
	r, session := forwardedApprovalRoom(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	f.tr.cardHook = func(ctx context.Context, target core.Room, _ core.Args) (string, error) {
		if target.DMUser != "owner" {
			t.Error("approval was forwarded to unexpected recipient", target)
		}
		close(entered)
		select {
		case <-release:
			return "acknowledged-approval", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan core.Args, 1)
	go func() {
		value, _ := f.g.ask(ctx, r, session, "Bash", core.Args{}, "Operation summary", nil, approvalPolicy{})
		done <- value
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("approval card send did not start")
	}
	eventually(t, func() bool { return hasApprovalNotice(f, "尚未确认送达") })
	if hasApprovalNotice(f, "已确认送达") || hasApprovalNotice(f, "已向维护者") {
		t.Fatal("approval reported sent before the platform acknowledged it")
	}
	close(release)
	eventually(t, func() bool { return hasApprovalNotice(f, "已确认送达至少一位") })
	f.g.approvalMu.Lock()
	bound := false
	for _, p := range f.g.approvals {
		bound = bound || p.cards["acknowledged-approval"] == "owner"
	}
	f.g.approvalMu.Unlock()
	if !bound {
		t.Fatal("delivery notice preceded the live card binding")
	}
	cancel()
	select {
	case value := <-done:
		if boolArg(value, "approved") {
			t.Fatal("delivery ACK was confused with human approval")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled approval remained waiting")
	}
}

func TestForwardedApprovalAllDeliveryFailuresEndRequest(t *testing.T) {
	f := newFixture(t, nil)
	r, session := forwardedApprovalRoom(t, f)
	f.tr.cardHook = func(context.Context, core.Room, core.Args) (string, error) {
		return "", errors.New("fixture delivery failure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, err := f.g.ask(ctx, r, session, "Bash", core.Args{}, "Operation summary", nil, approvalPolicy{})
	if err != nil || boolArg(value, "approved") || stringArg(value, "reason") != "card delivery failed" {
		t.Fatal(value, err)
	}
	eventually(t, func() bool { return hasApprovalNotice(f, "本次请求已结束，操作未获批准") })
	if hasApprovalNotice(f, "已确认送达") || hasApprovalNotice(f, "已向维护者") {
		t.Fatal("all failed delivery was reported as delivered")
	}
	if f.g.approvalCount(session) != 0 {
		t.Fatal("failed delivery retained a live approval")
	}
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM requests WHERE session=?", session).Scan(&state); err != nil || state != "denied" {
		t.Fatal("failed delivery was not durably denied", state, err)
	}
}

func TestForwardedApprovalLateCardDoesNotReportLiveDelivery(t *testing.T) {
	f := newFixture(t, nil)
	r, session := forwardedApprovalRoom(t, f)
	entered, release, disabled := make(chan struct{}), make(chan struct{}), make(chan struct{}, 2)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	settled := make(chan struct{}, 1)
	f.g.logger = func(event string, _ core.Args) {
		if event == "request_settled" {
			settled <- struct{}{}
		}
	}
	f.tr.cardHook = func(context.Context, core.Room, core.Args) (string, error) {
		close(entered)
		<-release
		return "late-forwarded-card", nil
	}
	f.tr.updateHook = func(_ context.Context, id string, card core.Args) error {
		if id == "late-forwarded-card" && strings.Contains(jsonText(card), "不再接受操作") {
			disabled <- struct{}{}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan core.Args, 1)
	go func() {
		value, _ := f.g.ask(ctx, r, session, "Bash", core.Args{}, "Operation summary", nil, approvalPolicy{})
		done <- value
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("approval card send did not start")
	}
	func() {
		// Hold durable settlement behind an explicit barrier. Returning to the
		// native caller and disabling a late card must not depend on that write.
		f.g.approvalMu.Lock()
		defer f.g.approvalMu.Unlock()
		cancel()
		select {
		case value := <-done:
			if boolArg(value, "approved") {
				t.Fatal("cancelled native request was approved")
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled approval waited for durable settlement")
		}
		close(release)
		select {
		case <-disabled:
		case <-time.After(time.Second):
			t.Fatal("late forwarded card was not disabled")
		}
	}()
	// Neither ask's return nor the transport's disabled-card ACK is a barrier
	// for the asynchronous database settlement. Observe its explicit outcome.
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("cancelled approval was not durably settled")
	}
	if hasApprovalNotice(f, "已确认送达") || f.g.approvalCount(session) != 0 {
		t.Fatal("late card acknowledgement revived a cancelled approval")
	}
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM requests WHERE session=?", session).Scan(&state); err != nil || state != "denied" {
		t.Fatal("cancelled approval was not durably denied", state, err)
	}
}

// Leave asynchronous settlement pending so cancellation checks cannot pass
// merely because another goroutine removed the approval from the map first.
func pendingApprovalFixture(t *testing.T, f *fixture, questions []question) (string, *approval) {
	t.Helper()
	r := f.room()
	session, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	id := core.NewID()
	kind := "permission"
	if questions != nil {
		kind = "question"
	}
	p := &approval{room: r, session: session.ID, kind: kind, revision: core.NewID(), expires: time.Now().Add(time.Minute).UnixMilli(), cards: map[string]string{}, questions: questions, outcome: make(chan core.Args, 1), ctx: ctx, cancel: cancel}
	if _, err := f.s.DB.Exec("INSERT INTO requests(id,room,session,kind,digest,state,expires) VALUES(?,?,?,?,?,'pending',?)", id, r.Key, session.ID, kind, "fixture", p.expires); err != nil {
		cancel()
		t.Fatal(err)
	}
	f.g.approvalMu.Lock()
	f.g.approvals[id] = p
	f.g.approvalMu.Unlock()
	t.Cleanup(func() {
		cancel()
		f.g.settleApproval(id, core.Args{"approved": false, "reason": "cancelled"}, "system")
	})
	return id, p
}

func TestCancelledApprovalCannotBindOrAcceptCallbacksBeforeSettlement(t *testing.T) {
	for _, action := range []string{"allow", "select", "submit", "answer"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t, nil)
			var questions []question
			if action != "allow" {
				questions = []question{{Question: "Pick", Options: []questionOption{{Label: "A"}}}}
			}
			id, p := pendingApprovalFixture(t, f, questions)
			if !f.g.bindApprovalCard(id, p, "current-card", p.room) {
				t.Fatal("live card was not bound")
			}
			p.cancel()
			if f.g.bindApprovalCard(id, p, "late-card", p.room) {
				t.Fatal("late ACK bound a cancelled approval before settlement")
			}
			f.g.approvalMu.Lock()
			_, late := p.cards["late-card"]
			present := f.g.approvals[id] == p
			f.g.approvalMu.Unlock()
			if late || !present {
				t.Fatal("cancellation test lost its pending-settlement boundary", late, present)
			}
			if action == "answer" {
				if reply := f.g.answer(p.room, "owner", id, "A"); reply != "问题不存在或已过期" {
					t.Fatal("cancelled question accepted a text answer", reply)
				}
			} else {
				value := core.Args{"request": id, "decision": action}
				if action == "select" {
					value = core.Args{"request": id, "question": 0, "option": 0}
				}
				reply, err := f.g.ActionResponse(context.Background(), core.CardAction{UserID: "owner", ChatID: p.room.ChatID, MessageID: "current-card", Value: value})
				if err != nil || stringArg(argsOf(reply["toast"]), "content") != "请求已结束或过期" || reply["card"] != nil {
					t.Fatal("cancelled approval accepted a callback", reply, err)
				}
			}
			// Reaching settlement with a previously validated positive value must
			// also recheck cancellation, independently of the callback entry guard.
			if f.g.settleApproval(id, core.Args{"approved": true, "answers": map[string]string{"Pick": "A"}}, "owner") {
				t.Fatal("cancelled decision was reported as accepted")
			}
			var state, actor string
			if err := f.s.DB.QueryRow("SELECT state,actor FROM requests WHERE id=?", id).Scan(&state, &actor); err != nil || state != "denied" || actor != "system" {
				t.Fatal("cancelled decision was not durably denied", state, actor, err)
			}
			select {
			case result := <-p.outcome:
				if boolArg(result, "approved") || stringArg(result, "reason") != "cancelled" {
					t.Fatal("cancelled decision authorized execution", result)
				}
			default:
				t.Fatal("cancelled decision did not release its native caller")
			}
		})
	}
}

func TestApprovalCancelledWhileWaitingForStorageCannotCommitLater(t *testing.T) {
	f := newFixture(t, nil)
	id, p := pendingApprovalFixture(t, f, nil)
	conn, err := f.s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := f.s.DB.Stats().WaitCount
	accepted, returned := make(chan bool, 1), make(chan struct{})
	go func() {
		defer close(returned)
		accepted <- f.g.settleApproval(id, core.Args{"approved": true}, "owner")
	}()
	defer func() {
		_ = conn.Close()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Error("settlement did not finish after releasing fixture storage")
		}
	}()
	// The store has one connection. This counter proves settlement is queued
	// for it before cancellation, without relying on a scheduling delay.
	eventually(t, func() bool { return f.s.DB.Stats().WaitCount > before })
	p.cancel()
	select {
	case result := <-p.outcome:
		if boolArg(result, "approved") {
			t.Fatal("cancelled storage wait authorized execution", result)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled positive decision remained queued for storage")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-accepted:
		if ok {
			t.Fatal("cancelled positive decision reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("settlement did not finish")
	}
	// The cancelled approval itself never commits; the request is then closed
	// durably as denied instead of staying pending until restart recovery.
	eventually(t, func() bool {
		var state, actor string
		err := f.s.DB.QueryRow("SELECT state,coalesce(actor,'') FROM requests WHERE id=?", id).Scan(&state, &actor)
		return err == nil && state == "denied" && actor == "system"
	})
}

func TestForwardedApprovalCancelledDuringDeliveryDoesNotReportFailure(t *testing.T) {
	f := newFixture(t, nil)
	r, session := forwardedApprovalRoom(t, f)
	entered := make(chan struct{}, 4)
	f.tr.cardHook = func(ctx context.Context, _ core.Room, _ core.Args) (string, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan core.Args, 1)
	go func() {
		value, _ := f.g.ask(ctx, r, session, "Bash", core.Args{}, "Operation summary", nil, approvalPolicy{})
		result <- value
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("approval card delivery did not start")
	}
	cancel()
	select {
	case value := <-result:
		if boolArg(value, "approved") || stringArg(value, "reason") != "cancelled" {
			t.Fatal(value)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled approval did not return")
	}
	eventually(t, func() bool { return f.g.approvalCount(session) == 0 })
	time.Sleep(100 * time.Millisecond)
	if hasApprovalNotice(f, "未能确认审批卡送达") {
		t.Fatal("cancellation was reported as an approval card delivery failure")
	}
}
