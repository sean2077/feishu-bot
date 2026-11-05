package gateway

import (
	"strings"
	"testing"

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
