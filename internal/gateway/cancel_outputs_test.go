package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestStopAfterModelFinishedClosesBlockedFileCardWithoutResend(t *testing.T) {
	f := newFixture(t, nil)
	f.tr.fileHook = func(context.Context, string) (string, error) { return "", classifiedFailure{} }
	f.send(f.message("stopped-file", "long answer", "member"))
	w := f.worker()
	d := f.delivery(w)
	answer := strings.Repeat("完整中文答案", 3000)
	f.finish(w, d, answer)
	var original string
	eventually(t, func() bool {
		return f.s.DB.QueryRow("SELECT id FROM outbox WHERE kind='reply' AND state='blocked' AND file_state='pending'").Scan(&original) == nil
	})
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	remainingRenders := len(s.renders)
	s.mu.Unlock()
	if remainingRenders != 0 {
		t.Fatal("fixture model did not finish before stop")
	}
	if f.tr.receiptDone("stopped-file") {
		t.Fatal("failed file was marked Done")
	}
	f.send(f.message("stop-file", "/stop", "owner"))
	eventually(t, func() bool {
		var count int
		_ = f.s.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='status' AND state='done' AND file_path IS NULL AND file_name IS NULL AND success=0").Scan(&count)
		return count == 1
	})
	o, err := f.g.output(original)
	if err != nil || o.State != "cancelled" || o.Text != answer {
		t.Fatalf("original output was not retained and cancelled: %+v %v", o, err)
	}
	if f.starts.Load() != 1 || f.tr.cardCount() != 1 || f.tr.receiptDone("stopped-file") {
		t.Fatal("stop created a native turn/card or marked failed delivery Done")
	}
	f.tr.mu.Lock()
	fileCalls := len(f.tr.files)
	last := f.tr.updates[len(f.tr.updates)-1]
	f.tr.mu.Unlock()
	_, color := replyCardStatus(t, last.card)
	visible := jsonText(last.card)
	if fileCalls != 1 || color != "grey" || argsOf(last.card["config"])["streaming_mode"] != false || !strings.Contains(visible, "此前已发出的请求仍可能到达") || strings.Contains(visible, "gateway_quick_retry") || strings.Contains(visible, "重试交付") || strings.Contains(visible, "完整答案另附文件") {
		t.Fatalf("cancelled card was misleading or resent file: file calls=%d card=%s", fileCalls, visible)
	}
	if ok, err := f.g.deliverOutput(context.Background(), original, f.room(), true); err != nil || ok {
		t.Fatalf("cancelled output was explicitly replayed: %t %v", ok, err)
	}
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	if len(f.tr.files) != fileCalls {
		t.Fatal("cancelled file was resent")
	}
}

func TestCancelPendingOutputsPreservesExistingStatusAndRollsBackAtomically(t *testing.T) {
	f := newFixture(t, nil)
	room := f.room()
	session, err := f.s.Session(room.Key)
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.g.saveOutput(f.s.DB, room, session.ID, "saved text", true, "known-card", "", "", DisplayCard("回复", "saved text"), nil, "", "", "reply", "blocked")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("CREATE TRIGGER cancel_status_failure BEFORE INSERT ON outbox WHEN NEW.kind='status' BEGIN SELECT RAISE(ABORT,'fixture rollback'); END"); err != nil {
		t.Fatal(err)
	}
	if err = f.g.cancelPendingOutputs(room, session.ID); err == nil {
		t.Fatal("status persistence failure should roll back cancellation")
	}
	if o, err := f.g.output(original); err != nil || o.State != "blocked" || o.Text != "saved text" {
		t.Fatal("cancellation escaped its transaction")
	}
	if _, err = f.s.DB.Exec("DROP TRIGGER cancel_status_failure"); err != nil {
		t.Fatal(err)
	}
	status, err := f.g.saveOutput(f.s.DB, room, session.ID, "already closing", false, "known-card", "", "", DisplayCard("已停止", "already closing"), nil, "", "", "status", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.g.cancelPendingOutputs(room, session.ID); err != nil {
		t.Fatal(err)
	}
	if o, err := f.g.output(status); err != nil || o.State != "pending" {
		t.Fatal("existing pending status was cancelled")
	}
	var count int
	if err = f.s.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='status'").Scan(&count); err != nil || count != 1 {
		t.Fatal("status close was duplicated")
	}
	if f.tr.cardCount() != 0 || f.starts.Load() != 0 {
		t.Fatal("cancellation created a message/native worker")
	}
}

func TestCancelledLegacyCardFitsWithoutSchedulingAnExport(t *testing.T) {
	card := DisplayCard("回复", strings.Repeat("中", 20000))
	card["header"] = core.Args{"title": core.Args{"tag": "plain_text", "content": strings.Repeat("旧标题", 20000)}}
	closed := cancelledOutputCard(card, strings.Repeat("原文", 20000))
	size, err := core.CardPayloadBytes(closed)
	if err != nil || size > core.CardPayloadLimit || argsOf(closed["config"])["streaming_mode"] != false || strings.Contains(jsonText(closed), "完整答案另附文件") {
		t.Fatalf("invalid cancellation preview: %d %v", size, err)
	}
}
