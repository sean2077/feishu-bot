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
		value, _ := f.g.ask(ctx, r, session, "Bash", core.Args{}, "Operation summary", nil)
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
	value, err := f.g.ask(ctx, r, session, "Bash", core.Args{}, "Operation summary", nil)
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
	done := make(chan struct{})
	go func() {
		_, _ = f.g.ask(ctx, r, session, "Bash", core.Args{}, "Operation summary", nil)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("approval card send did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled approval did not return")
	}
	close(release)
	select {
	case <-disabled:
	case <-time.After(time.Second):
		t.Fatal("late forwarded card was not disabled")
	}
	if hasApprovalNotice(f, "已确认送达") || f.g.approvalCount(session) != 0 {
		t.Fatal("late card acknowledgement revived a cancelled approval")
	}
}
