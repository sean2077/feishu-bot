package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func walkCardButtons(elements []any, visit func(core.Args)) {
	for _, raw := range elements {
		element := argsOf(raw)
		if stringArg(element, "tag") == "button" {
			visit(element)
		}
		children, _ := element["elements"].([]any)
		walkCardButtons(children, visit)
		columns, _ := element["columns"].([]any)
		for _, rawColumn := range columns {
			children, _ := argsOf(rawColumn)["elements"].([]any)
			walkCardButtons(children, visit)
		}
	}
}

func TestCardButtonsDeclareCallbackBehaviors(t *testing.T) {
	cards := []core.Args{
		approvalCard("request-id", &approval{kind: "permission", summary: "操作摘要"}, ""),
		approvalCard("request-id", &approval{
			kind:     "question",
			revision: "revision-id",
			questions: []question{{
				Question: "请选择",
				Options:  []questionOption{{Label: "A"}, {Label: "B"}},
			}},
		}, ""),
	}
	for _, card := range cards {
		body := argsOf(card["body"])
		elements, _ := body["elements"].([]any)
		buttons := 0
		walkCardButtons(elements, func(element core.Args) {
			buttons++
			if _, exists := element["value"]; exists {
				t.Fatalf("card JSON 2.0 button must not use a top-level value: %#v", element)
			}
			behaviors, ok := element["behaviors"].([]any)
			if !ok || len(behaviors) != 1 {
				t.Fatalf("button is missing a callback behavior: %#v", element)
			}
			behavior := argsOf(behaviors[0])
			value := argsOf(behavior["value"])
			if stringArg(behavior, "type") != "callback" || stringArg(value, "request") != "request-id" {
				t.Fatalf("button callback behavior is invalid: %#v", element)
			}
		})
		if buttons == 0 {
			t.Fatal("card has no interactive buttons")
		}
	}
}

func TestQuestionActionsShareOneRow(t *testing.T) {
	card := approvalCard("request-id", &approval{
		kind:      "question",
		revision:  "revision-id",
		questions: []question{{Question: "请选择", Options: []questionOption{{Label: "A"}}}},
	}, "")
	elements, _ := questionForm(card)["elements"].([]any)
	rows := 0
	for _, raw := range elements {
		element := argsOf(raw)
		if stringArg(element, "tag") != "column_set" {
			continue
		}
		rows++
		columns, _ := element["columns"].([]any)
		if len(columns) != 2 {
			t.Fatalf("action row has %d columns, want 2", len(columns))
		}
		labels := []string{}
		for _, rawColumn := range columns {
			children, _ := argsOf(rawColumn)["elements"].([]any)
			if len(children) != 1 {
				t.Fatalf("column has %d elements, want 1", len(children))
			}
			button := argsOf(children[0])
			if stringArg(button, "tag") != "button" || stringArg(button, "width") != "fill" {
				t.Fatalf("column does not contain a filled button: %#v", button)
			}
			if stringArg(button, "form_action_type") != "submit" || stringArg(button, "name") == "" {
				t.Fatalf("form button does not submit the local fields: %#v", button)
			}
			labels = append(labels, stringArg(argsOf(button["text"]), "content"))
		}
		if strings.Join(labels, ",") != "提交回答,取消" {
			t.Fatalf("action row labels = %v", labels)
		}
	}
	if rows != 1 {
		t.Fatalf("question card has %d action rows, want 1", rows)
	}
}

func TestQuestionSelectorsKeepChoicesLocalUntilSubmission(t *testing.T) {
	card := approvalCard("request-id", &approval{
		kind: "question", session: "session-id", revision: "revision-id",
		questions: []question{{Question: "请选择", Options: []questionOption{{Label: "A", Description: "选项说明"}, {Label: "B"}}}},
	}, "ou_user")
	elements, _ := questionForm(card)["elements"].([]any)
	var selector core.Args
	for _, raw := range elements {
		element := argsOf(raw)
		if stringArg(element, "tag") == "select_static" {
			selector = element
		}
	}
	if selector == nil || selector["behaviors"] != nil || selector["initial_option"] != nil || selector["selected_values"] != nil {
		t.Fatalf("selector must collect a fresh local draft without intermediate callbacks: %#v", selector)
	}
	options, _ := selector["options"].([]any)
	if len(options) != 2 || stringArg(argsOf(options[0]), "value") != "0" || stringArg(argsOf(options[1]), "value") != "1" {
		t.Fatalf("selector lost stable option identities: %#v", options)
	}
	value := cardValue(card, "提交回答")
	if value["session"] != "session-id" || value["revision"] != "revision-id" || value["request"] != "request-id" {
		t.Fatalf("form submission lost its request binding: %#v", value)
	}
}

func replyCardStatus(t *testing.T, card core.Args) (text, color string) {
	t.Helper()
	if _, exists := card["header"]; exists {
		t.Fatal("reply card must not declare a header")
	}
	elements, _ := argsOf(card["body"])["elements"].([]any)
	if len(elements) == 0 {
		t.Fatal("reply card has no body elements")
	}
	row := argsOf(elements[0])
	if stringArg(row, "tag") != "column_set" {
		t.Fatalf("first element tag = %q, want column_set", stringArg(row, "tag"))
	}
	columns, _ := row["columns"].([]any)
	if len(columns) != 2 {
		t.Fatalf("status row has %d columns, want 2", len(columns))
	}
	left, _ := argsOf(columns[0])["elements"].([]any)
	if len(left) != 1 || !strings.Contains(stringArg(argsOf(left[0]), "content"), "飞书助手") {
		t.Fatalf("status row has no assistant label: %#v", left)
	}
	right, _ := argsOf(columns[1])["elements"].([]any)
	if len(right) != 1 {
		t.Fatalf("status row has %d right elements, want 1", len(right))
	}
	textArgs := argsOf(argsOf(right[0])["text"])
	return stringArg(textArgs, "content"), stringArg(textArgs, "text_color")
}

func TestReplyCardUsesCompactStatusRowAndCollapsedThinking(t *testing.T) {
	card := replyCard("处理中", "", []string{"first", "second", "third"}, 5, false, replyDetails{Model: "example-model"}, time.Second, true)
	status, color := replyCardStatus(t, card)
	if status != "● 思考中" || color != "blue" {
		t.Fatalf("status = %q/%q, want 思考中/blue", status, color)
	}
	elements, _ := argsOf(card["body"])["elements"].([]any)
	if len(elements) != 3 {
		t.Fatalf("reply card has %d elements, want status/answer/reasoning", len(elements))
	}
	if answer := argsOf(elements[1]); stringArg(answer, "element_id") != "reply_answer" {
		t.Fatalf("second element is not reply_answer: %#v", answer)
	}
	panel := argsOf(elements[2])
	if stringArg(panel, "tag") != "collapsible_panel" || boolArg(panel, "expanded") {
		t.Fatalf("thinking panel = %#v, want a collapsed collapsible_panel", panel)
	}
	if _, exists := panel["background_color"]; exists {
		t.Fatal("thinking panel still has a background color")
	}
	if _, exists := panel["border"]; exists {
		t.Fatal("thinking panel still has a border")
	}
	title := argsOf(argsOf(panel["header"])["title"])
	if stringArg(title, "content") != "思考过程 · 5 步" {
		t.Fatalf("thinking title = %q", stringArg(title, "content"))
	}
	steps, _ := panel["elements"].([]any)
	if len(steps) != 4 {
		t.Fatalf("thinking panel has %d elements, want hidden notice + 3 steps", len(steps))
	}
	if hidden := stringArg(argsOf(argsOf(steps[0])["text"]), "content"); hidden != "另有 2 步未显示" {
		t.Fatalf("hidden-step notice = %q", hidden)
	}
}

func TestReplyCardStatusColors(t *testing.T) {
	cases := []struct {
		name, title, wantText, wantColor string
		final, success                   bool
		total                            int
	}{
		{"processing", "处理中", "● 处理中", "blue", false, true, 0},
		{"thinking", "处理中", "● 思考中", "blue", false, true, 1},
		{"waiting", "等待审批", "● 等待审批", "orange", false, true, 0},
		{"answered", "回复", "● 已回复", "green", true, true, 0},
		{"failed", "执行失败", "● 执行失败", "red", true, false, 0},
		{"stopped", "已停止", "● 已停止", "grey", true, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := replyCard(tc.title, "answer", nil, tc.total, tc.final, replyDetails{}, time.Second, tc.success)
			text, color := replyCardStatus(t, card)
			if text != tc.wantText || color != tc.wantColor {
				t.Fatalf("status = %q/%q, want %q/%q", text, color, tc.wantText, tc.wantColor)
			}
		})
	}
}

func TestReplyCardFooterIsCompactAndTerminalOnly(t *testing.T) {
	streaming := replyCard("处理中", "partial", nil, 0, false, replyDetails{Model: "m"}, 3*time.Second, true)
	if strings.Contains(jsonText(streaming), "⏱") {
		t.Fatal("streaming card must not render elapsed time")
	}
	final := replyCard("回复", "done", nil, 0, true, replyDetails{Model: "m", ProjectPath: "/work/demo"}, 3*time.Second, true)
	if strings.Contains(jsonText(final), `"tag":"hr"`) {
		t.Fatal("reply card must not declare its own divider")
	}
	var meta, path string
	elements, _ := argsOf(final["body"])["elements"].([]any)
	for _, raw := range elements {
		element := argsOf(raw)
		if stringArg(element, "tag") != "markdown" {
			continue
		}
		content := stringArg(element, "content")
		if strings.Contains(content, "⏱") {
			meta = content
		}
		if content == "/work/demo" {
			path = content
		}
	}
	if meta != "⏱ 3.0 秒 · m · ctx 未知" {
		t.Fatalf("metadata = %q", meta)
	}
	if path != "/work/demo" {
		t.Fatalf("project path = %q", path)
	}
}
