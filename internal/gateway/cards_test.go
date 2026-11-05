package gateway

import (
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestCardButtonsDeclareCallbackBehaviors(t *testing.T) {
	cards := []core.Args{
		approvalCard("request-id", &approval{kind: "permission", summary: "操作摘要"}),
		approvalCard("request-id", &approval{
			kind:     "question",
			revision: "revision-id",
			questions: []question{{
				Question: "请选择",
				Options:  []questionOption{{Label: "A"}, {Label: "B"}},
			}},
		}),
	}
	for _, card := range cards {
		body := argsOf(card["body"])
		elements, _ := body["elements"].([]any)
		buttons := 0
		for _, raw := range elements {
			element := argsOf(raw)
			if stringArg(element, "tag") != "button" {
				continue
			}
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
		}
		if buttons == 0 {
			t.Fatal("card has no interactive buttons")
		}
	}
}
