package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestStatusSummaryExtractsPhase(t *testing.T) {
	text := "**本聊天状态**\n状态：处理中\n项目：example\n"
	if got := statusSummary(text); got != "**本聊天状态** · 处理中" {
		t.Fatalf("summary = %q", got)
	}
	if got := statusSummary("**本聊天状态**\n项目：example\n"); got != "**本聊天状态**" {
		t.Fatalf("summary without phase = %q", got)
	}
}

func TestStatusCardIsSingleCollapsibleCard(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	f.g.refreshStatusCard(r, "member")
	f.tr.mu.Lock()
	if len(f.tr.cards) != 1 || len(f.tr.updates) != 0 {
		f.tr.mu.Unlock()
		t.Fatalf("cards=%d updates=%d, want one created card", len(f.tr.cards), len(f.tr.updates))
	}
	first := f.tr.cards[0]
	f.tr.mu.Unlock()
	if first.reply != "" || first.thread != "" {
		t.Fatalf("status card should be standalone, reply=%q thread=%q", first.reply, first.thread)
	}
	if argsOf(first.card["config"])["streaming_mode"] != false {
		t.Fatal("status card must not stream")
	}
	elements, _ := argsOf(first.card["body"])["elements"].([]any)
	if len(elements) != 2 {
		t.Fatalf("status card has %d elements, want summary and panel", len(elements))
	}
	summary := argsOf(elements[0])
	if stringArg(summary, "element_id") != "reply_answer" || !strings.Contains(stringArg(summary, "content"), "本聊天状态") {
		t.Fatalf("summary element = %#v", summary)
	}
	panel := argsOf(elements[1])
	if stringArg(panel, "tag") != "collapsible_panel" || !boolArg(panel, "expanded") {
		t.Fatalf("details panel = %#v", panel)
	}
	panelElements, _ := panel["elements"].([]any)
	details := argsOf(panelElements[0])
	if stringArg(details, "element_id") != "reply_status" || !strings.Contains(stringArg(details, "content"), "状态：") {
		t.Fatalf("detail element = %#v", details)
	}
	message, err := f.s.StatusCard(r.Key)
	if err != nil || message != first.id {
		t.Fatalf("stored status message = %q, %v; want %q", message, err, first.id)
	}

	f.g.refreshStatusCard(r, "member")
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	if len(f.tr.cards) != 1 || len(f.tr.updates) != 1 || f.tr.updates[0].id != message {
		t.Fatalf("refresh created another card: cards=%d updates=%#v", len(f.tr.cards), f.tr.updates)
	}
}

func TestStatusCardRefreshFailureDoesNotDuplicate(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	f.g.refreshStatusCard(r, "member")
	f.tr.mu.Lock()
	first := f.tr.cards[0].id
	f.tr.updateHook = func(context.Context, string, core.Args) error {
		return classifiedFailure{uncertain: true}
	}
	f.tr.mu.Unlock()
	f.g.refreshStatusCard(r, "member")
	f.tr.mu.Lock()
	if len(f.tr.cards) != 1 {
		f.tr.mu.Unlock()
		t.Fatalf("uncertain refresh created a duplicate card")
	}
	f.tr.updateHook = func(context.Context, string, core.Args) error {
		return classifiedFailure{}
	}
	f.tr.mu.Unlock()
	f.g.refreshStatusCard(r, "member")
	f.tr.mu.Lock()
	if len(f.tr.cards) != 2 {
		f.tr.mu.Unlock()
		t.Fatalf("definitive failure did not create a replacement; original=%s cards=%d", first, len(f.tr.cards))
	}
	replacement := f.tr.cards[1].id
	f.tr.mu.Unlock()
	if message, err := f.s.StatusCard(r.Key); err != nil || message != replacement {
		t.Fatalf("stored status message = %q, %v; want replacement %q", message, err, replacement)
	}
}
