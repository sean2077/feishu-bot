package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func cardContextHint(card core.Args) string {
	elements, _ := argsOf(card["body"])["elements"].([]any)
	for _, item := range elements {
		element := argsOf(item)
		if stringArg(element, "element_id") == replyContextHintElementID {
			return stringArg(element, "content")
		}
	}
	return ""
}

func (f *fixture) lastReplyCard() core.Args {
	var raw string
	err := f.s.DB.QueryRow("SELECT card FROM outbox WHERE room=? AND kind='reply' ORDER BY rowid DESC LIMIT 1", f.room().Key).Scan(&raw)
	if err != nil {
		return nil
	}
	var card core.Args
	if json.Unmarshal([]byte(raw), &card) != nil {
		return nil
	}
	return card
}

func waitReplyHint(t *testing.T, f *fixture, inputID, want string) core.Args {
	t.Helper()
	eventually(t, func() bool {
		return f.status(inputID) == "done" && cardContextHint(f.lastReplyCard()) == want
	})
	return f.lastReplyCard()
}

func (f *fixture) contextHintSent(session string) bool {
	var sent int
	if err := f.s.DB.QueryRow("SELECT coalesce((SELECT context_hint_sent FROM session_preferences WHERE session=? AND room=?),0)", session, f.room().Key).Scan(&sent); err != nil {
		f.t.Fatal(err)
	}
	return sent == 1
}

func emitContextResult(w *fakeWorker, d core.Delivery, usage core.Args) {
	w.emit(core.WorkerEvent{Type: "result", UUID: d.UUID, UUIDs: []string{d.UUID}, ResultID: core.NewID(), Text: "answer", Success: true, Source: "foreground", Usage: usage})
}

func emitCompactResult(w *fakeWorker, d core.Delivery) {
	w.emit(core.WorkerEvent{Type: "result", UUID: d.UUID, UUIDs: []string{d.UUID}, ResultID: core.NewID(), Success: true, Source: "foreground", Compact: "compacted", Usage: core.Args{"input_tokens": int64(500000)}})
}

func TestContextHintOncePerSessionAndResetsAfterCompactAndNew(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Projects[0].ContextHintTokens = 10000 })
	usage := core.Args{"input_tokens": int64(6000), "cache_read_input_tokens": int64(3000), "cache_creation_input_tokens": int64(1000)}
	f.send(f.message("one", "first", "member"))
	w := f.worker()
	d1 := f.delivery(w)
	emitContextResult(w, d1, usage)
	card := waitReplyHint(t, f, "one", "上下文约 1 万 token，建议 /compact 或 /new")
	if !f.contextHintSent(d1.SessionID) {
		t.Fatal("first hint was not marked")
	}

	f.send(f.message("two", "second", "member"))
	d2 := f.delivery(w)
	emitContextResult(w, d2, usage)
	waitReplyHint(t, f, "two", "")
	if !strings.Contains(jsonText(card), replyContextHintElementID) {
		t.Fatal("first card lost its hint")
	}

	f.send(f.message("compact", "/compact", "owner"))
	compact := f.delivery(w)
	emitCompactResult(w, compact)
	waitReplyHint(t, f, "compact", "")
	if f.contextHintSent(d1.SessionID) {
		t.Fatal("compact did not clear the session hint marker")
	}

	f.send(f.message("three", "third", "member"))
	d3 := f.delivery(w)
	emitContextResult(w, d3, core.Args{"input_tokens": int64(15000)})
	waitReplyHint(t, f, "three", "上下文约 2 万 token，建议 /compact 或 /new")
	if !f.contextHintSent(d3.SessionID) {
		t.Fatal("post-compact hint was not marked")
	}

	f.send(f.message("new", "/new", "owner"))
	f.send(f.message("four", "fourth", "member"))
	w2 := f.worker()
	d4 := f.delivery(w2)
	if d4.SessionID == d3.SessionID {
		t.Fatal("/new did not rotate the session")
	}
	emitContextResult(w2, d4, core.Args{"input_tokens": int64(15000)})
	waitReplyHint(t, f, "four", "上下文约 2 万 token，建议 /compact 或 /new")
	if !f.contextHintSent(d4.SessionID) {
		t.Fatal("new session hint was not marked")
	}
}

func TestContextHintUsesProjectDefaultAndStaysBelowThreshold(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("below", "first", "member"))
	w := f.worker()
	d := f.delivery(w)
	emitContextResult(w, d, core.Args{"input_tokens": int64(399999)})
	waitReplyHint(t, f, "below", "")
	if f.contextHintSent(d.SessionID) {
		t.Fatal("below-threshold turn marked the hint")
	}

	f.send(f.message("at", "second", "member"))
	d2 := f.delivery(w)
	emitContextResult(w, d2, core.Args{"input_tokens": int64(400000)})
	waitReplyHint(t, f, "at", "上下文约 40 万 token，建议 /compact 或 /new")
	if !f.contextHintSent(d2.SessionID) {
		t.Fatal("default threshold did not mark the hint")
	}
}

func TestContextHintCoexistsWithAutoNotice(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].PermissionMode = "auto"
		c.Projects[0].ContextHintTokens = 10000
	})
	f.permissionMode = "auto"
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "activity", Phase: "tool_start", Tool: "Bash", ToolUseID: "tool-1"})
	w.emit(core.WorkerEvent{Type: "auto_denied", Phase: "classifier", ToolUseID: "tool-1", Text: "Blocked by classifier"})
	emitContextResult(w, d, core.Args{"input_tokens": int64(10000)})
	card := waitReplyHint(t, f, "one", "上下文约 1 万 token，建议 /compact 或 /new")
	text := jsonText(card)
	if strings.Count(text, "reply_auto_notice") != 1 || strings.Count(text, replyContextHintElementID) != 1 {
		t.Fatalf("notice and hint did not coexist: %s", text)
	}
	if !strings.Contains(text, "安全分类器拦截了 Bash") {
		t.Fatalf("auto notice was overwritten: %s", text)
	}
}
