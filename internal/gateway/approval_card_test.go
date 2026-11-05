package gateway

import (
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestOversizedQuestionCardFitsWithoutDroppingOptions(t *testing.T) {
	long := strings.Repeat("很长的说明", 4000)
	p := &approval{
		kind:     "question",
		room:     core.Room{Key: "room"},
		revision: "revision",
		selected: map[string]map[int]map[int]bool{},
		questions: []question{{
			Question: long,
			Header:   long,
			Options: []questionOption{
				{Label: long, Description: long},
				{Label: "短选项"},
			},
		}},
	}
	card := approvalCard("request", p, "")
	size, err := core.CardPayloadBytes(card)
	if err != nil || size > core.CardPayloadLimit {
		t.Fatalf("oversized question card: size=%d err=%v", size, err)
	}
	text := jsonText(card)
	if !strings.Contains(text, "提交选择") || !strings.Contains(text, "取消") {
		t.Fatal("submit controls were dropped while fitting the card")
	}
	if !strings.Contains(text, `"option":0`) || !strings.Contains(text, `"option":1`) {
		t.Fatal("option values were dropped while fitting the card")
	}
	if !strings.Contains(text, "…") {
		t.Fatal("expected bounded display text")
	}
}

func TestOversizedPermissionCardFitsAndKeepsButtons(t *testing.T) {
	p := &approval{kind: "permission", summary: strings.Repeat("命令参数", 10000)}
	card := approvalCard("request", p, "")
	size, err := core.CardPayloadBytes(card)
	if err != nil || size > core.CardPayloadLimit {
		t.Fatalf("oversized permission card: size=%d err=%v", size, err)
	}
	text := jsonText(card)
	if !strings.Contains(text, "批准一次") || !strings.Contains(text, "拒绝") {
		t.Fatal("approval buttons were dropped while fitting the card")
	}
	if !strings.Contains(text, "…") {
		t.Fatal("expected bounded summary text")
	}
}
