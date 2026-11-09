package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func actionApprovalFixture(t *testing.T, approvers, owners []string, forbid bool) *fixture {
	t.Helper()
	return newFixture(t, func(c *core.Config) {
		c.Projects[0].Owners = owners
		c.Projects[0].Approvals.ForbidSelfApproval = forbid
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish", Executable: "fixture", Approvers: approvers}}
	})
}

func fakeActionSuccess(f *fixture) {
	f.g.actionRunner = func(context.Context, string, []string, string, map[string]string, int64) (int, string, error) {
		return 0, "", nil
	}
}

func approvalCardFor(t *testing.T, f *fixture) (sentCard, core.Args) {
	t.Helper()
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	value := cardValue(card.card, "批准一次")
	if value == nil {
		t.Fatal("approval card missing allow button")
	}
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	return card, value
}

func clickApproval(t *testing.T, f *fixture, card sentCard, value core.Args, user string) string {
	t.Helper()
	text, err := f.g.Action(context.Background(), core.CardAction{UserID: user, ChatID: card.room.ChatID, MessageID: card.id, Value: value})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func startActionApproval(t *testing.T, f *fixture, sender string) (sentCard, core.Args, *fakeWorker) {
	t.Helper()
	fakeActionSuccess(f)
	message := f.message("message-"+sender, "work", sender)
	f.send(message)
	worker := f.worker()
	_ = f.delivery(worker)
	worker.emit(core.WorkerEvent{Type: "request", ID: "action-request", Kind: "action", Name: "publish", Input: core.Args{}})
	card, value := approvalCardFor(t, f)
	return card, value, worker
}

func TestApproversReplaceMaintainers(t *testing.T) {
	f := actionApprovalFixture(t, []string{"member"}, nil, false)
	card, value, worker := startActionApproval(t, f, "member")
	text := jsonText(card.card)
	if !strings.Contains(text, "需要指定审批人批准") || !strings.Contains(text, "审批人：指定审批人") {
		t.Fatalf("card did not identify designated approvers: %s", text)
	}
	if got := clickApproval(t, f, card, value, "owner"); got != "仅指定审批人可以审批" {
		t.Fatalf("maintainer outside approvers: %q", got)
	}
	var rejected int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='approval_self_rejected'").Scan(&rejected); err != nil || rejected != 0 {
		t.Fatalf("non-approver rejection wrote self audit: %d %v", rejected, err)
	}
	if got := clickApproval(t, f, card, value, "member"); got != "已批准一次" {
		t.Fatalf("designated approver: %q", got)
	}
	if got := response(t, worker); !boolArg(got, "ok") {
		t.Fatalf("action result: %#v", got)
	}
}

func TestForbidSelfApprovalBlocksRequesterButKeepsRequestPending(t *testing.T) {
	f := actionApprovalFixture(t, []string{"owner", "member"}, []string{"owner"}, true)
	fakeActionSuccess(f)
	message := f.message("message-member", "work", "member")
	message.SenderName = "Alice"
	f.send(message)
	worker := f.worker()
	_ = f.delivery(worker)
	worker.emit(core.WorkerEvent{Type: "request", ID: "action-request", Kind: "action", Name: "publish", Input: core.Args{}})
	card, value := approvalCardFor(t, f)
	request := stringArg(value, "request")
	if !strings.Contains(jsonText(card.card), "发起人：Alice") || !strings.Contains(jsonText(card.card), "`member`") {
		t.Fatalf("approval card omitted requester: %s", jsonText(card.card))
	}
	if got := clickApproval(t, f, card, value, "member"); got != selfApprovalToast {
		t.Fatalf("self approval: %q", got)
	}
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM requests WHERE id=?", request).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("self approval settled request: %s %v", state, err)
	}
	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='approval_self_rejected' ORDER BY seq DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal([]byte(detail), &audit); err != nil {
		t.Fatal(err)
	}
	if audit["request"] != request || audit["user"] != "member" || audit["reason"] != "self" || strings.Contains(detail, "Alice") {
		t.Fatalf("self rejection audit = %#v", audit)
	}
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("owner approval: %q", got)
	}
	if got := response(t, worker); !boolArg(got, "ok") {
		t.Fatalf("action result: %#v", got)
	}
	var requester, requestContext, actor sql.NullString
	if err := f.s.DB.QueryRow("SELECT requester, request_context, actor FROM requests WHERE id=?", request).Scan(&requester, &requestContext, &actor); err != nil {
		t.Fatal(err)
	}
	if !requester.Valid || requester.String != "member" || !requestContext.Valid || !actor.Valid || actor.String != "owner" {
		t.Fatalf("request row = %#v %#v %#v", requester, requestContext, actor)
	}
	var stored core.RequestContext
	if err := json.Unmarshal([]byte(requestContext.String), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.MessageID != "message-member" || strings.Join(stored.Senders, ",") != "member" {
		t.Fatalf("stored request context = %#v", stored)
	}
}

func TestMultipleSendersAreAllSelfApprovers(t *testing.T) {
	f := actionApprovalFixture(t, []string{"owner", "member", "second"}, []string{"owner"}, true)
	fakeActionSuccess(f)
	f.send(f.message("message-member", "work", "member"))
	worker := f.worker()
	_ = f.delivery(worker)
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	s.deliveries["extra"] = core.Delivery{UUID: "extra", Message: core.Message{ID: "message-second", ChatID: "group", ChatType: "group", SenderID: "second"}}
	s.order = append(s.order, "extra")
	s.mu.Unlock()
	worker.emit(core.WorkerEvent{Type: "request", ID: "action-request", Kind: "action", Name: "publish", Input: core.Args{}})
	card, value := approvalCardFor(t, f)
	for _, user := range []string{"member", "second"} {
		if got := clickApproval(t, f, card, value, user); got != selfApprovalToast {
			t.Fatalf("%s self approval: %q", user, got)
		}
	}
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("owner approval: %q", got)
	}
	if got := response(t, worker); !boolArg(got, "ok") {
		t.Fatalf("action result: %#v", got)
	}
}

func TestOwnerCanApproveOwnRequest(t *testing.T) {
	f := actionApprovalFixture(t, []string{"owner"}, []string{"owner"}, true)
	card, value, worker := startActionApproval(t, f, "owner")
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("owner self approval: %q", got)
	}
	if got := response(t, worker); !boolArg(got, "ok") {
		t.Fatalf("action result: %#v", got)
	}
}

func TestUnknownRequesterOnlyOwnerCanApprove(t *testing.T) {
	f := actionApprovalFixture(t, []string{"owner", "member"}, []string{"owner"}, true)
	fakeActionSuccess(f)
	session, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan core.Args, 1)
	go func() {
		value, _ := f.g.executeAction(context.Background(), f.room(), session.ID, f.c.Projects[0], "publish", core.Args{}, core.Workspace{}, nil)
		done <- value
	}()
	card, value := approvalCardFor(t, f)
	if got := clickApproval(t, f, card, value, "member"); got != unknownRequesterToast {
		t.Fatalf("non-owner with unknown requester: %q", got)
	}
	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='approval_self_rejected' ORDER BY seq DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal([]byte(detail), &audit); err != nil {
		t.Fatal(err)
	}
	if audit["request"] != stringArg(value, "request") || audit["user"] != "member" || audit["reason"] != "unknown_requester" {
		t.Fatalf("unknown requester audit = %#v", audit)
	}
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("owner with unknown requester: %q", got)
	}
	select {
	case result := <-done:
		if !boolArg(result, "ok") {
			t.Fatalf("action result: %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("action did not finish")
	}
}

func TestRequesterCanDenyOwnRequest(t *testing.T) {
	f := actionApprovalFixture(t, []string{"owner", "member"}, []string{"owner"}, true)
	fakeActionSuccess(f)
	f.send(f.message("message-member", "work", "member"))
	worker := f.worker()
	_ = f.delivery(worker)
	worker.emit(core.WorkerEvent{Type: "request", ID: "action-request", Kind: "action", Name: "publish", Input: core.Args{}})
	card, value := approvalCardFor(t, f)
	deny := core.Args{"request": value["request"], "decision": "deny"}
	if got := clickApproval(t, f, card, deny, "member"); got != "已拒绝" {
		t.Fatalf("requester deny: %q", got)
	}
	if got := response(t, worker); boolArg(got, "ok") || stringArg(got, "error") != "denied" {
		t.Fatalf("action result: %#v", got)
	}
}

func TestNativeToolAskAppliesSelfApprovalAndCapturesContext(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Maintainers = []string{"owner", "member", "second"}
		c.Projects[0].Owners = []string{"owner"}
		c.Projects[0].Approvals.ForbidSelfApproval = true
	})
	f.send(f.message("message-member", "work", "member"))
	worker := f.worker()
	_ = f.delivery(worker)
	worker.emit(core.WorkerEvent{Type: "request", ID: "permission-request", Kind: "permission", Name: "Bash", Input: core.Args{"command": "fixed-command"}})
	card, value := approvalCardFor(t, f)
	cardText := jsonText(card.card)
	if !strings.Contains(cardText, "工具：Bash") || !strings.Contains(cardText, "发起人：`member`") || !strings.Contains(cardText, "需要维护者批准") {
		t.Fatalf("permission card summary = %s", cardText)
	}
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	s.deliveries["extra"] = core.Delivery{UUID: "extra", Message: core.Message{ID: "message-second", ChatID: "group", ChatType: "group", SenderID: "second"}}
	s.order = append(s.order, "extra")
	s.mu.Unlock()
	if got := clickApproval(t, f, card, value, "member"); got != selfApprovalToast {
		t.Fatalf("native self approval: %q", got)
	}
	if got := clickApproval(t, f, card, value, "second"); got != "已批准一次" {
		t.Fatalf("context was re-read during approval: %q", got)
	}
	if got := response(t, worker); !boolArg(got, "approved") {
		t.Fatalf("native approval result: %#v", got)
	}
}

func TestQuestionFormBypassesApprovalPolicy(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Maintainers = []string{"owner", "member"}
		c.Projects[0].Owners = []string{"owner"}
		c.Projects[0].Approvals.ForbidSelfApproval = true
	})
	f.send(f.message("message-member", "work", "member"))
	worker := f.worker()
	_ = f.delivery(worker)
	worker.emit(core.WorkerEvent{Type: "request", ID: "question-request", Kind: "question", Name: "AskUserQuestion", Input: core.Args{"questions": []any{core.Args{"question": "Pick?", "options": []any{core.Args{"label": "A"}}}}}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	submit := cardValue(card.card, "提交回答")
	if submit == nil {
		t.Fatal("question card missing submit")
	}
	waitApprovalBinding(t, f, card, stringArg(submit, "request"))
	if text, err := f.g.Action(context.Background(), core.CardAction{UserID: "member", ChatID: card.room.ChatID, MessageID: card.id, Value: submit, FormValue: core.Args{"question_0": "0"}}); err != nil || text != "已提交" {
		t.Fatalf("question submit = %q %v", text, err)
	}
	if got := response(t, worker); !boolArg(got, "approved") {
		t.Fatalf("question result: %#v", got)
	}
}

func TestAnswerCommandBypassesApprovalPolicy(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Maintainers = []string{"owner", "member"}
		c.Projects[0].Owners = []string{"owner"}
		c.Projects[0].Approvals.ForbidSelfApproval = true
	})
	f.send(f.message("message-member", "work", "member"))
	worker := f.worker()
	_ = f.delivery(worker)
	worker.emit(core.WorkerEvent{Type: "request", ID: "question-request", Kind: "question", Name: "AskUserQuestion", Input: core.Args{"questions": []any{core.Args{"question": "Pick?", "options": []any{core.Args{"label": "A"}}}}}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	value := cardValue(card.card, "提交回答")
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	if text := f.g.answer(f.room(), "member", stringArg(value, "request"), "A"); text != "回答已提交" {
		t.Fatalf("/answer result = %q", text)
	}
	if got := response(t, worker); !boolArg(got, "approved") {
		t.Fatalf("question result: %#v", got)
	}
}

func TestForwardedApprovalUsesDesignatedApprovers(t *testing.T) {
	f := newFixture(t, nil)
	r, session := forwardedApprovalRoom(t, f)
	done := make(chan core.Args, 1)
	go func() {
		value, _ := f.g.ask(context.Background(), r, session, "publish", core.Args{}, "Operation summary", nil, approvalPolicy{approvers: []string{"owner"}, designated: true})
		done <- value
	}()
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	if card.room.DMUser != "owner" || card.room.ChatID != "owner" {
		t.Fatalf("forwarded to %#v", card.room)
	}
	value := cardValue(card.card, "批准一次")
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	if !hasApprovalNotice(f, "指定审批人") {
		t.Fatal("forwarded notice did not mention designated approvers")
	}
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("designated approver in DM: %q", got)
	}
	select {
	case result := <-done:
		if !boolArg(result, "approved") {
			t.Fatalf("forwarded result: %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forwarded approval did not finish")
	}
}

func TestGroupApproverOutsideAuthorizedAuthorsCannotApprove(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish", Executable: "fixture", Approvers: []string{"outsider"}}}
	})
	fakeActionSuccess(f)
	f.send(f.message("message-member", "work", "member"))
	worker := f.worker()
	_ = f.delivery(worker)
	worker.emit(core.WorkerEvent{Type: "request", ID: "action-request", Kind: "action", Name: "publish", Input: core.Args{}})
	card, value := approvalCardFor(t, f)
	if got := clickApproval(t, f, card, value, "outsider"); got != "" {
		t.Fatalf("unauthorized approver received a toast: %q", got)
	}
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM requests WHERE id=?", stringArg(value, "request")).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("unauthorized approval settled the request: %s %v", state, err)
	}
}
