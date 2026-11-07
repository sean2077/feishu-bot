package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func questionForm(card core.Args) core.Args {
	elements, _ := argsOf(card["body"])["elements"].([]any)
	for _, raw := range elements {
		if element := argsOf(raw); stringArg(element, "tag") == "form" {
			return element
		}
	}
	return nil
}

func TestQuestionCardHasOneFormWithOptionalCustomAnswerPerQuestion(t *testing.T) {
	p := &approval{kind: "question", session: "session", revision: "revision", questions: []question{
		{Question: "Choose a release", Options: []questionOption{{Label: "Stable", Description: "Recommended"}, {Label: "Preview"}}},
		{Question: "Choose targets", MultiSelect: true, Options: []questionOption{{Label: "Linux"}, {Label: "macOS"}}},
		{Question: "Anything else?"},
	}}
	card := approvalCard("request", p, "")
	form := questionForm(card)
	if form == nil {
		t.Fatal("question card has no form for inline custom answers")
	}
	forms := 0
	body, _ := argsOf(card["body"])["elements"].([]any)
	for _, raw := range body {
		if stringArg(argsOf(raw), "tag") == "form" {
			forms++
		}
	}
	if forms != 1 {
		t.Fatalf("all questions must share one submission: got %d forms", forms)
	}
	elements, _ := form["elements"].([]any)
	fields := map[string]core.Args{}
	for _, raw := range elements {
		e := argsOf(raw)
		if name := stringArg(e, "name"); name != "" {
			fields[name] = e
		}
	}
	for name, tag := range map[string]string{"question_0": "select_static", "question_1": "multi_select_static", "custom_0": "input", "custom_1": "input", "custom_2": "input"} {
		if stringArg(fields[name], "tag") != tag || boolArg(fields[name], "required") {
			t.Fatalf("field %s must be an optional %s: %#v", name, tag, fields[name])
		}
		if tag == "input" {
			if limit, ok := numberArg(fields[name], "max_length"); !ok || limit != 1000 {
				t.Fatalf("custom input has no matching client-side length bound: %#v", fields[name])
			}
		}
	}
	if fields["question_2"] != nil {
		t.Fatal("free-text-only question rendered an empty selector")
	}
	if !strings.Contains(jsonText(card), "Recommended") || !strings.Contains(jsonText(card), "/answer request") {
		t.Fatal("card lost option descriptions or the text-command fallback")
	}
}

func TestQuestionFormSubmitsCustomAndMultipleChoicesTogether(t *testing.T) {
	f := newFixture(t, nil)
	id, p := pendingApprovalFixture(t, f, []question{
		{Question: "Release", Options: []questionOption{{Label: "Stable"}, {Label: "Preview"}}},
		{Question: "Targets", MultiSelect: true, Options: []questionOption{{Label: "Linux"}, {Label: "macOS"}, {Label: "Windows"}}},
		{Question: "Notes"},
	})
	p.revision = "question-revision"
	if !f.g.bindApprovalCard(id, p, "question-card", p.room) {
		t.Fatal("question card was not bound")
	}
	a := core.CardAction{UserID: "member", ChatID: p.room.ChatID, MessageID: "question-card", Value: core.Args{"request": id, "session": p.session, "revision": p.revision, "decision": "submit"}, FormValue: core.Args{
		"question_0": "0", "custom_0": "  Use the maintenance branch  ",
		"question_1": []any{"2", "0"}, "custom_1": " \n ",
		"custom_2": "/approve request-id; /new",
	}}
	text, err := f.g.Action(context.Background(), a)
	if err != nil || text != "已提交" {
		t.Fatalf("inline question answers were not accepted: %q %v", text, err)
	}
	answer := <-p.outcome
	values := argsOf(answer["answers"])
	if !boolArg(answer, "approved") || values["Release"] != "Use the maintenance branch" || values["Targets"] != "Linux, Windows" || values["Notes"] != "/approve request-id; /new" {
		t.Fatalf("custom priority, blank fallback, multi-choice order or literal text changed: %#v", answer)
	}
	var actor string
	if err := f.s.DB.QueryRow("SELECT actor FROM requests WHERE id=? AND state='approved'", id).Scan(&actor); err != nil || actor != "member" {
		t.Fatalf("form submission did not preserve the authenticated actor: %q %v", actor, err)
	}
}

func TestQuestionFormRejectsInvalidFieldsWithoutSettling(t *testing.T) {
	for name, form := range map[string]core.Args{
		"blank":                {"custom_0": " \n\t", "custom_1": "\u3000"},
		"wrong custom type":    {"custom_0": []any{"not text"}},
		"too long":             {"custom_0": strings.Repeat("字", 1001)},
		"single choice array":  {"question_0": []any{"0"}},
		"single choice number": {"question_0": 0},
		"out of range":         {"question_0": "2"},
		"negative":             {"question_0": "-1"},
		"multi choice string":  {"custom_0": "answer", "question_1": "0"},
		"multi choice number":  {"custom_0": "answer", "question_1": []any{0}},
		"duplicate choice":     {"custom_0": "answer", "question_1": []any{"0", "0"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			id, p := pendingApprovalFixture(t, f, []question{
				{Question: "First", Options: []questionOption{{Label: "A"}, {Label: "B"}}},
				{Question: "Second", MultiSelect: true, Options: []questionOption{{Label: "X"}, {Label: "Y"}}},
			})
			if !f.g.bindApprovalCard(id, p, "question-card", p.room) {
				t.Fatal("question card was not bound")
			}
			a := core.CardAction{UserID: "member", ChatID: p.room.ChatID, MessageID: "question-card", Value: core.Args{"request": id, "session": p.session, "revision": p.revision, "decision": "submit"}, FormValue: form}
			reply, err := f.g.ActionResponse(context.Background(), a)
			if err != nil || reply["card"] != nil || stringArg(argsOf(reply["toast"]), "content") == "已提交" {
				t.Fatalf("invalid form was accepted or replaced the local draft: %#v %v", reply, err)
			}
			assertQuestionPending(t, f, id, p)
			a.FormValue = core.Args{"question_0": "1", "question_1": []any{"0", "1"}}
			if text, err := f.g.Action(context.Background(), a); err != nil || text != "已提交" {
				t.Fatalf("corrected form was rejected: %q %v", text, err)
			}
		})
	}
}

func assertQuestionPending(t *testing.T, f *fixture, id string, p *approval) {
	t.Helper()
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM requests WHERE id=?", id).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("rejected form changed durable request: state=%s err=%v", state, err)
	}
	select {
	case result := <-p.outcome:
		t.Fatalf("rejected form reached native: %#v", result)
	default:
	}
}

func TestQuestionFormRejectsChangedBindings(t *testing.T) {
	for _, change := range []string{"request", "message", "chat", "actor", "revision", "missing revision", "session", "missing session", "permission decision"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t, nil)
			id, p := pendingApprovalFixture(t, f, []question{{Question: "Question"}})
			if !f.g.bindApprovalCard(id, p, "question-card", p.room) {
				t.Fatal("question card was not bound")
			}
			a := core.CardAction{UserID: "member", ChatID: p.room.ChatID, MessageID: "question-card", Value: core.Args{"request": id, "session": p.session, "revision": p.revision, "decision": "submit"}, FormValue: core.Args{"custom_0": "answer"}}
			switch change {
			case "request", "revision", "session":
				a.Value[change] = "another-" + change
			case "message":
				a.MessageID = "another-card"
			case "chat":
				a.ChatID = "another-chat"
			case "actor":
				a.UserID = "stranger"
			case "missing revision":
				delete(a.Value, "revision")
			case "missing session":
				delete(a.Value, "session")
			case "permission decision":
				a.Value["decision"] = "allow"
			}
			if text, err := f.g.Action(context.Background(), a); err != nil || text == "已提交" || text == "已批准一次" {
				t.Fatalf("changed binding accepted: %q %v", text, err)
			}
			assertQuestionPending(t, f, id, p)
		})
	}
}

func TestQuestionFormCannotApprovePermission(t *testing.T) {
	f := newFixture(t, nil)
	id, p := pendingApprovalFixture(t, f, nil)
	if !f.g.bindApprovalCard(id, p, "permission-card", p.room) {
		t.Fatal("permission card was not bound")
	}
	a := core.CardAction{UserID: "owner", ChatID: p.room.ChatID, MessageID: "permission-card", Value: core.Args{"request": id, "decision": "submit", "session": p.session, "revision": p.revision}, FormValue: core.Args{"custom_0": "/approve " + id, "decision": "allow", "approved": true}}
	if text, err := f.g.Action(context.Background(), a); err != nil || text != "无效决定" {
		t.Fatalf("question form was treated as permission approval: %q %v", text, err)
	}
	assertQuestionPending(t, f, id, p)
}

func TestExpiredQuestionFormDoesNotReachNative(t *testing.T) {
	f := newFixture(t, nil)
	id, p := pendingApprovalFixture(t, f, []question{{Question: "Question"}})
	if !f.g.bindApprovalCard(id, p, "question-card", p.room) {
		t.Fatal("question card was not bound")
	}
	p.expires = time.Now().Add(-time.Second).UnixMilli()
	a := core.CardAction{UserID: "member", ChatID: p.room.ChatID, MessageID: "question-card", Value: core.Args{"request": id, "session": p.session, "revision": p.revision, "decision": "submit"}, FormValue: core.Args{"custom_0": "answer"}}
	if text, err := f.g.Action(context.Background(), a); err != nil || text != "请求已结束或过期" {
		t.Fatalf("expired question remained actionable: %q %v", text, err)
	}
	assertQuestionPending(t, f, id, p)
}

func TestQuestionFormSessionSwitchWhileDecisionWaits(t *testing.T) {
	f := newFixture(t, nil)
	id, p := pendingApprovalFixture(t, f, []question{{Question: "Question"}})
	if !f.g.bindApprovalCard(id, p, "question-card", p.room) {
		t.Fatal("question card was not bound")
	}
	conn, err := f.s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before := f.s.DB.Stats().WaitCount
	done := make(chan string, 1)
	go func() {
		text, err := f.g.Action(context.Background(), core.CardAction{UserID: "member", ChatID: p.room.ChatID, MessageID: "question-card", Value: core.Args{"request": id, "session": p.session, "revision": p.revision, "decision": "submit"}, FormValue: core.Args{"custom_0": "answer"}})
		if err != nil {
			t.Error(err)
		}
		done <- text
	}()
	eventually(t, func() bool { return f.s.DB.Stats().WaitCount > before })
	if _, err := conn.ExecContext(context.Background(), "UPDATE sessions SET state='archived' WHERE id=?", p.session); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-done:
		if text != "请求已结束或过期" {
			t.Fatalf("archived session accepted a queued answer: %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("question callback did not finish")
	}
	if value := <-p.outcome; boolArg(value, "approved") || value["answers"] != nil {
		t.Fatalf("stale question answer reached native: %#v", value)
	}
	var approved int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM requests WHERE id=? AND state='approved'", id).Scan(&approved); err != nil || approved != 0 {
		t.Fatalf("stale session approved durably: %d %v", approved, err)
	}
}

func TestQuestionTextAnswerFallbackStillWorks(t *testing.T) {
	for _, multi := range []bool{false, true} {
		name := "single"
		if multi {
			name = "multiple"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.send(f.message("task", "task", "member"))
			w := f.worker()
			_ = f.delivery(w)
			questions := []any{core.Args{"question": "First"}}
			text := "Literal /new answer"
			if multi {
				questions = append(questions, core.Args{"question": "Second"})
				text = `{"First":"Literal /new answer","Second":"Another answer"}`
			}
			w.emit(core.WorkerEvent{Type: "request", ID: "question", Kind: "question", Name: "AskUserQuestion", Input: core.Args{"questions": questions}})
			eventually(t, func() bool { return f.tr.cardCount() == 1 })
			card := f.tr.cardLast()
			request := stringArg(cardValue(card.card, "提交回答"), "request")
			waitApprovalBinding(t, f, card, request)
			f.send(f.message("text-answer", "/answer "+request+" "+text, "member"))
			answers := argsOf(response(t, w)["answers"])
			if answers["First"] != "Literal /new answer" || (multi && answers["Second"] != "Another answer") {
				t.Fatalf("text-command fallback lost its answers: %#v", answers)
			}
		})
	}
}
