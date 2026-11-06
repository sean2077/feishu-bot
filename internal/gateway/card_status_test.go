package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// Frozen shape from the first compact-card release, before reply_status was
// assigned. Do not build this fixture with the current replyStatusRow helper.
func savedUnmarkedReplyCard(t *testing.T) core.Args {
	t.Helper()
	const saved = `{"schema":"2.0","config":{"streaming_mode":false},"body":{"elements":[{"tag":"column_set","flex_mode":"none","horizontal_spacing":"medium","columns":[{"tag":"column","width":"weighted","weight":1,"vertical_align":"center","elements":[{"tag":"markdown","content":"**飞书助手**"}]},{"tag":"column","width":"auto","horizontal_align":"right","vertical_align":"center","elements":[{"tag":"div","text":{"tag":"plain_text","text_size":"notation","text_color":"green","content":"● 已回复"}}]}]},{"tag":"markdown","element_id":"reply_answer","content":"保存的完整正文"}]}}`
	var card core.Args
	if err := json.Unmarshal([]byte(saved), &card); err != nil {
		t.Fatal(err)
	}
	return card
}

// The transport race hooks cannot call Fatal from their worker goroutine.
// Read the actual visible status row, not an obsolete header template.
func visibleReplyStatusColor(card core.Args) string {
	elements, _ := argsOf(card["body"])["elements"].([]any)
	if len(elements) == 0 {
		return ""
	}
	columns, _ := argsOf(elements[0])["columns"].([]any)
	if len(columns) != 2 {
		return ""
	}
	right, _ := argsOf(columns[1])["elements"].([]any)
	if len(right) != 1 {
		return ""
	}
	return stringArg(argsOf(argsOf(right[0])["text"]), "text_color")
}

func TestDeliveryStatusUpdatesSavedCardFormats(t *testing.T) {
	cases := []struct {
		name string
		card func(*testing.T) core.Args
	}{
		{"current", func(*testing.T) core.Args {
			return replyCard("回复", "保存的完整正文", nil, 0, true, replyDetails{}, time.Second, true)
		}},
		{"current-streaming", func(*testing.T) core.Args {
			return replyCard("处理中", "保存的完整正文", nil, 0, false, replyDetails{}, time.Second, false)
		}},
		{"unmarked-compact", savedUnmarkedReplyCard},
		{"unmarked-compact-with-header", func(t *testing.T) core.Args {
			card := savedUnmarkedReplyCard(t)
			card["header"] = core.Args{"template": "orange", "title": core.Args{"tag": "plain_text", "content": "文件待送达"}}
			return card
		}},
		{"legacy-header", func(*testing.T) core.Args { return DisplayCard("回复", "保存的完整正文") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			r := f.room()
			s, err := f.s.Session(r.Key)
			if err != nil {
				t.Fatal(err)
			}
			id, err := f.g.saveOutput(f.s.DB, r, s.ID, "保存的完整正文", true, "existing-card", "", "", tc.card(t), nil, "", "", "reply", "pending")
			if err != nil {
				t.Fatal(err)
			}
			o, err := f.g.output(id)
			if err != nil {
				t.Fatal(err)
			}
			check := func(card core.Args, text, color string) {
				t.Helper()
				gotText, gotColor := replyCardStatus(t, card)
				if gotText != "● "+text || gotColor != color {
					t.Fatalf("visible status = %q/%q, want %q/%q", gotText, gotColor, text, color)
				}
				wire := jsonText(card)
				if !strings.Contains(wire, "保存的完整正文") || strings.Contains(wire, "● 已回复") || strings.Contains(wire, "● 处理中") {
					t.Fatalf("card lost its answer or retained a conflicting status: %s", wire)
				}
				if size, err := core.CardPayloadBytes(card); err != nil || size > core.CardPayloadLimit {
					t.Fatalf("status card exceeds the wire budget: %d %v", size, err)
				}
			}
			check(f.g.outputCard(o, r, true), "模型已完成 · 文件待送达", "orange")
			f.g.pendingOutputNotice(context.Background(), o, r, o.Target, "交付仍待确认")
			f.tr.mu.Lock()
			if len(f.tr.updates) != 1 {
				f.tr.mu.Unlock()
				t.Fatal("pending notice did not update exactly the known card")
			}
			updated := f.tr.updates[0]
			f.tr.mu.Unlock()
			if updated.id != o.Target {
				t.Fatal("pending notice changed the target card")
			}
			check(updated.card, "回复尚未完整送达", "orange")
			check(cancelledOutputCard(o.Card, o.Text), "已停止 · 交付取消", "grey")
			latest, err := f.g.output(id)
			if err != nil || latest.State != "pending" || latest.Text != "保存的完整正文" || f.starts.Load() != 0 || f.tr.cardCount() != 0 {
				t.Fatalf("status rendering changed delivery/native state: %+v %v", latest, err)
			}
		})
	}
}

func TestStatusCompatibilityPreservesCustomElements(t *testing.T) {
	card := savedUnmarkedReplyCard(t)
	elements := argsOf(card["body"])["elements"].([]any)
	custom := argsOf(elements[0])
	columns := custom["columns"].([]any)
	left := argsOf(columns[0])["elements"].([]any)
	argsOf(left[0])["content"] = "**自定义信息**"
	right := argsOf(columns[1])["elements"].([]any)
	argsOf(argsOf(right[0])["text"])["content"] = "● 外部数据"
	want := jsonText(custom)
	closed := cancelledOutputCard(card, "保存的完整正文")
	gotText, gotColor := replyCardStatus(t, closed)
	if gotText != "● 已停止 · 交付取消" || gotColor != "grey" {
		t.Fatal("custom card did not get an explicit cancellation status")
	}
	count := 0
	for _, value := range argsOf(closed["body"])["elements"].([]any) {
		if jsonText(value) == want {
			count++
		}
	}
	if count != 1 || !strings.Contains(jsonText(closed), "保存的完整正文") {
		t.Fatal("status compatibility replaced unrelated custom content")
	}
}
