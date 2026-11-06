package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestSupplementReceiptCardFormNamesAuthorAndFencesContent(t *testing.T) {
	text := "第一行\n```go\nfmt.Println(\"x\")\n```\n最后一行"
	got := supplementReceipt(core.Message{SenderID: "ou_member", InteractionMessageID: "om_card", Text: text})
	if !strings.Contains(got, `<at user_id="ou_member">补充者</at>`) {
		t.Fatalf("receipt lost the author mention: %q", got)
	}
	if !strings.Contains(got, text) {
		t.Fatalf("receipt lost the full supplement: %q", got)
	}
	if strings.Contains(got, "不代表模型已读取") {
		t.Fatalf("receipt still contains the removed disclaimer: %q", got)
	}
	lines := strings.Split(got, "\n")
	fence := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			fence = line
			break
		}
	}
	if fence != "````" {
		t.Fatalf("fence = %q, want four backticks for content containing three", fence)
	}
}

func TestSupplementReceiptBtwStaysAttributionOnly(t *testing.T) {
	got := supplementReceipt(core.Message{SenderID: "ou_member", Text: "只看当前失败用例"})
	if !strings.Contains(got, `<at user_id="ou_member">补充者</at>`) {
		t.Fatalf("btw receipt lost the author mention: %q", got)
	}
	if strings.Contains(got, "只看当前失败用例") {
		t.Fatalf("btw receipt duplicated the visible message: %q", got)
	}
	if !strings.Contains(got, "补充已交给原生输入流") {
		t.Fatalf("btw receipt lost the delivery status: %q", got)
	}
	if strings.Contains(got, "不代表模型已读取") {
		t.Fatalf("btw receipt still contains the removed disclaimer: %q", got)
	}
}

func TestSupplementReceiptSanitizesAndHandlesEmpty(t *testing.T) {
	if got := supplementReceipt(core.Message{SenderID: "ou_member", InteractionMessageID: "om_card", Text: " \x00 "}); got != "" {
		t.Fatalf("control-only receipt = %q, want empty", got)
	}
	got := supplementReceipt(core.Message{SenderID: "ou_member", InteractionMessageID: "om_card", Text: "a\x00b"})
	if strings.ContainsRune(got, '\x00') || !strings.Contains(got, "ab") {
		t.Fatalf("receipt was not sanitized: %q", got)
	}
}

func TestAcceptedCardSupplementPostsAuthorReceipt(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	supp := core.Delivery{
		UUID: core.NewID(), RoomKey: f.room().Key, SessionID: s.ID, SupplementTo: "turn",
		Message: core.Message{
			ID: "card:supplement", InteractionMessageID: "om_card", ChatID: f.room().ChatID,
			ChatType: "group", SenderID: "member", Text: "补充正文", Time: time.Now().UnixMilli(),
		},
	}
	slot := f.g.getSlot(f.room().Key)
	if slot == nil {
		t.Fatal("room slot missing")
	}
	slot.mu.Lock()
	slot.deliveries[supp.UUID] = supp
	slot.mu.Unlock()
	w.emit(core.WorkerEvent{Type: "accepted", UUID: supp.UUID})
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, `<at user_id="member">补充者</at>`) && strings.Contains(notice, "补充正文") {
				return true
			}
		}
		return false
	})
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	for _, notice := range f.tr.notices {
		if strings.Contains(notice, "不代表模型已读取") {
			t.Fatalf("accepted receipt still contains the removed disclaimer: %q", notice)
		}
	}
}
