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
	body := argsOf(card["body"])
	elements, _ := body["elements"].([]any)
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
			labels = append(labels, stringArg(argsOf(button["text"]), "content"))
		}
		if strings.Join(labels, ",") != "提交选择,取消" {
			t.Fatalf("action row labels = %v", labels)
		}
	}
	if rows != 1 {
		t.Fatalf("question card has %d action rows, want 1", rows)
	}
}

func TestQuestionOptionsUseCompactRows(t *testing.T) {
	selected := map[string]map[int]map[int]bool{"ou_user": {0: {0: true}}}
	questions := []question{{Question: "请选择", Options: []questionOption{{Label: "A", Description: "选项说明"}, {Label: "B"}}}}
	private := approvalCard("request-id", &approval{
		kind: "question", revision: "revision-id", room: core.Room{DMUser: "ou_user"},
		questions: questions, selected: selected,
	}, "ou_user")
	group := approvalCard("request-id", &approval{
		kind: "question", revision: "revision-id", questions: questions,
	}, "ou_user")
	find := func(card core.Args) (core.Args, core.Args) {
		elements, _ := argsOf(card["body"])["elements"].([]any)
		for _, raw := range elements {
			element := argsOf(raw)
			if stringArg(element, "tag") != "column_set" {
				continue
			}
			columns, _ := element["columns"].([]any)
			if len(columns) != 2 {
				continue
			}
			left, _ := argsOf(columns[0])["elements"].([]any)
			right, _ := argsOf(columns[1])["elements"].([]any)
			if len(right) != 1 || stringArg(argsOf(right[0]), "tag") != "button" || stringArg(argsOf(argsOf(right[0])["text"]), "content") != "选择" {
				continue
			}
			if len(left) == 0 {
				t.Fatal("option row has no label")
			}
			return argsOf(left[0]), argsOf(right[0])
		}
		return nil, nil
	}
	label, action := find(private)
	if label == nil || action == nil {
		t.Fatal("private option row is missing")
	}
	if !strings.Contains(stringArg(label, "content"), "✓ A") || stringArg(action, "type") != "primary" {
		t.Fatalf("private selection is not highlighted: label=%v action=%v", label, action)
	}
	label, action = find(group)
	if label == nil || action == nil {
		t.Fatal("group option row is missing")
	}
	if strings.Contains(stringArg(label, "content"), "✓") || stringArg(action, "type") != "default" {
		t.Fatalf("group card disclosed a selection: label=%v action=%v", label, action)
	}
}

func TestQuestionSelectionSummaryOnlyForPrivateCards(t *testing.T) {
	questions := []question{
		{Question: "第一题", Options: []questionOption{{Label: "A"}, {Label: "B"}, {Label: "C"}}},
		{Question: "第二题", MultiSelect: true, Options: []questionOption{{Label: "X"}, {Label: "Y"}}},
	}
	selected := map[string]map[int]map[int]bool{
		"ou_user": {0: {0: true, 2: true}, 1: {1: true}},
	}
	private := approvalCard("request-id", &approval{
		kind: "question", revision: "revision-id", room: core.Room{DMUser: "ou_user"},
		questions: questions, selected: selected,
	}, "ou_user")
	group := approvalCard("request-id", &approval{
		kind: "question", revision: "revision-id",
		questions: questions, selected: selected,
	}, "ou_user")
	intro := func(card core.Args) string {
		elements, _ := argsOf(card["body"])["elements"].([]any)
		return stringArg(argsOf(elements[0]), "content")
	}
	if text := intro(private); !strings.Contains(text, "当前选择") || !strings.Contains(text, "1. A, C") || !strings.Contains(text, "2. Y") {
		t.Fatalf("private card is missing the current selection: %q", text)
	}
	if text := intro(group); strings.Contains(text, "当前选择") {
		t.Fatalf("group card disclosed a member's selection: %q", text)
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
