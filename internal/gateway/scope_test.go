package gateway

import (
	"context"
	"encoding/json"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
	"testing"
	"time"
)

func cardValue(card core.Args, label string) core.Args {
	body := argsOf(card["body"])
	elements, _ := body["elements"].([]any)
	var find func([]any) core.Args
	find = func(children []any) core.Args {
		for _, value := range children {
			element := argsOf(value)
			if stringArg(element, "tag") == "button" && stringArg(argsOf(element["text"]), "content") == label {
				behaviors, _ := element["behaviors"].([]any)
				for _, raw := range behaviors {
					behavior := argsOf(raw)
					if stringArg(behavior, "type") == "callback" {
						return argsOf(behavior["value"])
					}
				}
			}
			children, _ := element["elements"].([]any)
			if value := find(children); value != nil {
				return value
			}
			columns, _ := element["columns"].([]any)
			for _, rawColumn := range columns {
				children, _ := argsOf(rawColumn)["elements"].([]any)
				if value := find(children); value != nil {
					return value
				}
			}
		}
		return nil
	}
	return find(elements)
}
func (t *fakeTransport) latestCard(id string) core.Args {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.updates) - 1; i >= 0; i-- {
		if t.updates[i].id == id {
			return t.updates[i].card
		}
	}
	for _, c := range t.cards {
		if c.id == id {
			return c.card
		}
	}
	return nil
}
func response(t *testing.T, w *fakeWorker) core.Args {
	t.Helper()
	select {
	case a := <-w.responses:
		return a
	case <-time.After(3 * time.Second):
		t.Fatal("native response not returned")
		return nil
	}
}

// waitApprovalBinding waits until the exact delivered card is registered. The
// transport records the card before the gateway installs the callback binding,
// so a click issued from cardCount() alone can race registration.
func waitApprovalBinding(t *testing.T, f *fixture, card sentCard, request string) {
	t.Helper()
	eventually(t, func() bool {
		f.g.approvalMu.Lock()
		defer f.g.approvalMu.Unlock()
		p := f.g.approvals[request]
		if p == nil {
			return false
		}
		_, observed := p.cards[card.id]
		return observed
	})
}
func TestPermissionApprovalsBindExactCardRoomAndMaintainer(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "req", Kind: "permission", Name: "Bash", Input: core.Args{"command": "fixed-command"}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	value := cardValue(card.card, "批准一次")
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	if value == nil {
		t.Fatal("permission card missing button")
	}
	for _, a := range []core.CardAction{{UserID: "owner", ChatID: "other", MessageID: card.id, Value: value}, {UserID: "owner", ChatID: "group", MessageID: "foreign-card", Value: value}, {UserID: "owner", ChatID: "group", MessageID: "", Value: value}} {
		text, e := f.g.Action(context.Background(), a)
		if e != nil || text != "" {
			t.Fatalf("foreign callback accepted: %q %v", text, e)
		}
	}
	text, e := f.g.Action(context.Background(), core.CardAction{UserID: "member", ChatID: "group", MessageID: card.id, Value: value})
	if e != nil || text != "仅维护者可以审批" {
		t.Fatalf("member permission: %q %v", text, e)
	}
	text, e = f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: value})
	if e != nil || text != "已批准一次" {
		t.Fatalf("owner approval: %q %v", text, e)
	}
	a := response(t, w)
	if !boolArg(a, "approved") {
		t.Fatal("approved response not forwarded")
	}
	text, e = f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: value})
	if e != nil || text != "请求已结束或过期" {
		t.Fatal("duplicate callback remained actionable")
	}
}
func TestForwardedApprovalCannotBeClaimedByOriginalDMUser(t *testing.T) {
	f := newFixture(t, nil)
	m := f.message("dm", "task", "member")
	m.ChatType = "p2p"
	m.ChatID = "dm-member"
	m.Mentioned = false
	f.send(m)
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "req", Kind: "permission", Name: "Bash", Input: core.Args{}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	if card.room.DMUser != "owner" {
		t.Fatalf("not forwarded to maintainer: %#v", card.room)
	}
	value := cardValue(card.card, "批准一次")
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	if value == nil {
		t.Fatal("permission card missing button")
	}
	// The fake transport records the card before Card returns. Wait for ask
	// to bind that delivery before testing callback authorization.
	eventually(t, func() bool {
		f.g.approvalMu.Lock()
		defer f.g.approvalMu.Unlock()
		p := f.g.approvals[stringArg(value, "request")]
		return p != nil && p.cards[card.id] == "owner"
	})
	text, _ := f.g.Action(context.Background(), core.CardAction{UserID: "member", ChatID: "dm-member", MessageID: card.id, Value: value})
	if text != "" {
		t.Fatal("requesting DM user claimed forwarded approval")
	}
	text, _ = f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "dm-owner", MessageID: "other", Value: value})
	if text != "" {
		t.Fatal("maintainer approved unobserved forwarded card")
	}
	text, e := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "dm-owner", MessageID: card.id, Value: value})
	if e != nil || text != "已批准一次" {
		t.Fatalf("correct forwarded approval rejected: text=%q error=%v", text, e)
	}
	if answer := response(t, w); !boolArg(answer, "approved") {
		t.Fatalf("forwarded approval response was not approved: %#v", answer)
	}
}
func TestQuestionFormDoesNotMergeDifferentActorsDrafts(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "task", "member"))
	w := f.worker()
	_ = f.delivery(w)
	questions := []any{core.Args{"question": "First?", "options": []any{core.Args{"label": "A"}, core.Args{"label": "B"}}}, core.Args{"question": "Second?", "multiSelect": true, "options": []any{core.Args{"label": "X"}, core.Args{"label": "Y"}}}}
	w.emit(core.WorkerEvent{Type: "request", ID: "q", Kind: "question", Name: "AskUserQuestion", Input: core.Args{"questions": questions}})
	eventually(t, func() bool { return f.tr.cardCount() == 1 })
	card := f.tr.cardLast()
	value := cardValue(card.card, "提交回答")
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	submit := func(actor string, form core.Args) string {
		t.Helper()
		reply, err := f.g.ActionResponse(context.Background(), core.CardAction{UserID: actor, ChatID: "group", MessageID: card.id, Value: value, FormValue: form})
		if err != nil || reply["card"] != nil {
			t.Fatalf("submission should return only a toast, preserving client drafts: %#v %v", reply, err)
		}
		return stringArg(argsOf(reply["toast"]), "content")
	}
	if text := submit("member", core.Args{"question_0": "0"}); text != "请先回答第 2 题，再提交。" {
		t.Fatalf("incomplete member form was accepted: %q", text)
	}
	if text := submit("second", core.Args{"question_1": []any{"1"}}); text != "请先回答第 1 题，再提交。" {
		t.Fatalf("another actor inherited the first actor's answer: %q", text)
	}
	if text := submit("member", core.Args{"question_0": "0", "question_1": []any{"0"}}); text != "已提交" {
		t.Fatalf("complete form was not submitted: %q", text)
	}
	answer := response(t, w)
	values := argsOf(answer["answers"])
	if values["First?"] != "A" || values["Second?"] != "X" {
		t.Fatalf("mixed actor choices: %#v", values)
	}
	if text := submit("second", core.Args{"custom_0": "replacement", "custom_1": "replacement"}); text != "请求已结束或过期" {
		t.Fatalf("second submission replaced the first: %q", text)
	}
	select {
	case repeated := <-w.responses:
		t.Fatalf("replayed form reached native: %#v", repeated)
	default:
	}
}

func TestPrivateQuestionFormRejectsAnotherActor(t *testing.T) {
	f := newFixture(t, nil)
	m := f.message("dm", "task", "member")
	m.ChatType, m.ChatID, m.Mentioned = "p2p", "dm-member", false
	f.send(m)
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "q", Kind: "question", Name: "AskUserQuestion", Input: core.Args{"questions": []any{core.Args{"question": "Pick", "options": []any{core.Args{"label": "A"}, core.Args{"label": "B"}}}}}})
	eventually(t, func() bool { return f.tr.cardCount() == 1 })
	card := f.tr.cardLast()
	value := cardValue(card.card, "提交回答")
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	a := core.CardAction{UserID: "owner", ChatID: "dm-member", MessageID: card.id, Value: value, FormValue: core.Args{"custom_0": "Private answer"}}
	if text, err := f.g.Action(context.Background(), a); err != nil || text != "" {
		t.Fatalf("another authorized user's private form was accepted: %q %v", text, err)
	}
	a.UserID = "member"
	if text, err := f.g.Action(context.Background(), a); err != nil || text != "已提交" {
		t.Fatalf("private form owner was rejected: %q %v", text, err)
	}
	if answers := argsOf(response(t, w)["answers"]); answers["Pick"] != "Private answer" {
		t.Fatalf("private custom answer was lost: %#v", answers)
	}
}

func TestStoppedApprovalAndOldCardCannotRunCommand(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "task", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "req", Kind: "permission", Name: "Bash", Input: core.Args{}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	value := cardValue(card.card, "批准一次")
	waitApprovalBinding(t, f, card, stringArg(value, "request"))
	f.send(f.message("stop", "/stop", "owner"))
	text, e := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: value})
	if e != nil || text != "请求已结束或过期" {
		t.Fatal("stopped card still live")
	}
	var state string
	_ = f.s.DB.QueryRow("SELECT state FROM requests WHERE id=?", value["request"]).Scan(&state)
	if state != "denied" {
		t.Fatalf("approval persisted as %s", state)
	}
}
func TestHistoryFiltersBeforeRememberingAndCursorBindsSession(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	good := f.message("good", "allowed", "member")
	good.ThreadID = "known-thread"
	good.Resources = []core.Resource{{Key: "good-resource", Kind: "file", Name: "allowed.txt"}}
	bad := f.message("bad", "hidden", "stranger")
	bad.Resources = []core.Resource{{Key: "bad-resource", Kind: "file", Name: "hidden.txt"}}
	other := f.message("foreign", "cross-chat", "member")
	other.ChatID = "other"
	self := f.message("self", "own bot output", "")
	self.SenderIsBot = true
	self.SenderIsSelf = true
	foreignBot := self
	foreignBot.ID = "foreign-bot"
	foreignBot.SenderIsSelf = false
	f.tr.historyPage = core.HistoryPage{Messages: []core.Message{good, bad, other, self, foreignBot}, NextPageToken: "native-next"}
	page, e := f.g.history(context.Background(), r, "session-a", core.Args{"limit": 5})
	if e != nil {
		t.Fatal(e)
	}
	messages := page["messages"].([]core.Message)
	if len(messages) != 2 || messages[0].ID != "good" || messages[1].ID != "self" {
		t.Fatalf("scope filter: %#v", messages)
	}
	m, _ := f.s.ContextMessage(r.Key, "bad")
	if m != nil {
		t.Fatal("unauthorized metadata was remembered")
	}
	m, _ = f.s.ContextMessage(r.Key, "good")
	if m == nil || m.Text != "" || len(m.Resources) != 1 {
		t.Fatal("remembered context retained text or lost resource metadata")
	}
	cursor := stringArg(page, "nextCursor")
	if _, e = f.g.history(context.Background(), r, "session-b", core.Args{"cursor": cursor}); e == nil {
		t.Fatal("cursor crossed session")
	}
	foreign, _ := RoomFor(f.c, "other", "")
	if _, e = f.g.history(context.Background(), foreign, "session-a", core.Args{"cursor": cursor}); e == nil {
		t.Fatal("cursor crossed chat")
	}
	if _, e = f.g.history(context.Background(), r, "session-a", core.Args{"cursor": cursor, "limit": 1}); e == nil {
		t.Fatal("cursor combined with changed query")
	}
	if _, e = f.g.history(context.Background(), r, "session-a", core.Args{"threadId": "unseen"}); e == nil {
		t.Fatal("unobserved thread fetched")
	}
	if _, e = f.g.history(context.Background(), r, "session-a", core.Args{"threadId": "known-thread"}); e != nil {
		t.Fatal(e)
	}
}
func TestUsageDeduplicatesActualResultIDsWithoutInventingTotals(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "task", "member"))
	w := f.worker()
	d := f.delivery(w)
	ev := core.WorkerEvent{Type: "result", UUID: d.UUID, Success: true, ResultID: "stable-result", Models: []string{"alpha"}, Usage: core.Args{"input_tokens": int64(10), "output_tokens": int64(2)}, Text: "done"}
	w.emit(ev)
	w.emit(ev)
	f.send(f.message("two", "next", "member"))
	d2 := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "result", UUID: d2.UUID, Success: false, ResultID: "second-result", Models: []string{"beta"}, Usage: core.Args{"input_tokens": int64(5)}, Text: "failed"})
	report, e := f.g.usageReport(f.room())
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "stable-result") {
		t.Fatal("latest result not selected")
	}
	sum := argsOf(report["cumulative"])
	u := argsOf(sum["usage"])
	subtotal := argsOf(sum["reportedSubtotal"])
	if sum["results"] != int64(2) || u["input_tokens"] != int64(15) || u["output_tokens"] != nil || subtotal["output_tokens"] != int64(2) || report["cost"] != nil || report["contextOccupancy"] != nil {
		t.Fatalf("incorrect token semantics: %s", raw)
	}
}

func TestApprovalCardArrivingAfterStopIsDisabled(t *testing.T) {
	f := newFixture(t, nil)
	entered, release, disabled := make(chan struct{}), make(chan struct{}), make(chan struct{}, 2)
	f.tr.cardHook = func(_ context.Context, _ core.Room, _ core.Args) (string, error) {
		close(entered)
		<-release
		return "late-approval", nil
	}
	f.tr.updateHook = func(_ context.Context, id string, card core.Args) error {
		if id == "late-approval" && strings.Contains(jsonText(card), "不再接受操作") {
			disabled <- struct{}{}
		}
		return nil
	}
	f.send(f.message("one", "task", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "permission", Kind: "permission", Name: "Bash", Input: core.Args{}})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("approval not sent")
	}
	if e := f.g.Message(context.Background(), f.message("stop", "/stop", "owner")); e != nil {
		t.Fatal(e)
	}
	close(release)
	select {
	case <-disabled:
	case <-time.After(time.Second):
		t.Fatal("late approval card stayed visually actionable")
	}
	f.g.approvalMu.Lock()
	remaining := len(f.g.approvals)
	f.g.approvalMu.Unlock()
	if remaining != 0 {
		t.Fatal("stopped request retained callback binding")
	}
}
