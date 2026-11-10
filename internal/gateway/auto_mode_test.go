package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestPermissionModeMismatchIsAuditedAndVisible(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Projects[0].PermissionMode = "auto" })
	f.permissionMode = "acceptEdits"
	f.send(f.message("one", "hello", "member"))
	f.worker()
	eventually(t, func() bool {
		var count int
		_ = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='permission_mode_mismatch'", f.room().Key).Scan(&count)
		return count == 1
	})
	view, err := f.g.statusView(f.room())
	if err != nil {
		t.Fatal(err)
	}
	mode := argsOf(view["permissionMode"])
	if stringArg(mode, "requested") != "auto" || stringArg(mode, "effective") != "acceptEdits" {
		t.Fatalf("status permission mode = %#v", view["permissionMode"])
	}
}

func TestAutoDeniedAuditsAndAddsOneCardNotice(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Projects[0].PermissionMode = "auto" })
	f.permissionMode = "auto"
	f.send(f.message("one", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "text", Text: "working", TurnID: d.UUID})
	for _, toolID := range []string{"tool-1", "tool-2"} {
		w.emit(core.WorkerEvent{Type: "activity", Phase: "tool_start", Tool: "Bash", ToolUseID: toolID})
		w.emit(core.WorkerEvent{Type: "activity", Phase: "tool_end", ToolUseID: toolID})
		w.emit(core.WorkerEvent{Type: "auto_denied", Phase: "classifier", ToolUseID: toolID, Text: "Blocked by classifier"})
	}
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, update := range f.tr.updates {
			text := jsonText(update.card)
			if strings.Contains(text, "安全分类器拦截了 Bash") {
				return strings.Count(text, "reply_auto_notice") == 1
			}
		}
		return false
	})
	var denied, unavailable int
	_ = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='auto_mode_denied'", f.room().Key).Scan(&denied)
	_ = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='auto_mode_unavailable'", f.room().Key).Scan(&unavailable)
	if denied != 2 || unavailable != 0 {
		t.Fatalf("auto audits denied=%d unavailable=%d", denied, unavailable)
	}
	var normalDetail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE room=? AND event='auto_mode_denied' ORDER BY seq LIMIT 1", f.room().Key).Scan(&normalDetail); err != nil {
		t.Fatal(err)
	}
	var normal map[string]any
	if err := json.Unmarshal([]byte(normalDetail), &normal); err != nil {
		t.Fatal(err)
	}
	if _, ok := normal["stage2Error"]; ok {
		t.Fatalf("normal classifier denial carried stage2Error: %#v", normal)
	}
}

func TestAutoUnavailableAuditsKindAndNotice(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Projects[0].PermissionMode = "auto" })
	f.permissionMode = "auto"
	f.send(f.message("one", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "text", Text: "working", TurnID: d.UUID})
	w.emit(core.WorkerEvent{Type: "activity", Phase: "tool_start", Tool: "Bash", ToolUseID: "tool-1"})
	w.emit(core.WorkerEvent{Type: "auto_denied", Phase: "unevaluable", ToolUseID: "tool-1", Text: "could not evaluate this action"})
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, update := range f.tr.updates {
			if strings.Contains(jsonText(update.card), "自动模式安全审查不可用，Bash 未执行") {
				return true
			}
		}
		return false
	})
	var kind, tool, reason string
	if err := f.s.DB.QueryRow("SELECT json_extract(detail,'$.kind'),json_extract(detail,'$.tool'),json_extract(detail,'$.reason') FROM audit WHERE room=? AND event='auto_mode_unavailable'", f.room().Key).Scan(&kind, &tool, &reason); err != nil {
		t.Fatal(err)
	}
	if kind != "unevaluable" || tool != "Bash" || reason != "could not evaluate this action" {
		t.Fatalf("unavailable audit = %q %q %q", kind, tool, reason)
	}
}

func TestAutoModeStage2ErrorAuditFlag(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Projects[0].PermissionMode = "auto" })
	f.permissionMode = "auto"
	f.send(f.message("one", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "text", Text: "working", TurnID: d.UUID})
	w.emit(core.WorkerEvent{Type: "activity", Phase: "tool_start", Tool: "Bash", ToolUseID: "tool-stage2"})
	w.emit(core.WorkerEvent{Type: "auto_denied", Phase: "classifier", ToolUseID: "tool-stage2", Text: "Stage 2 classifier error - blocking based on stage 1 assessment (usually transient)", Stage2Error: true})
	eventually(t, func() bool {
		var count int
		_ = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='auto_mode_denied'", f.room().Key).Scan(&count)
		return count == 1
	})
	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE room=? AND event='auto_mode_denied'", f.room().Key).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["stage2Error"] != true || fields["tool"] != "Bash" {
		t.Fatalf("stage2 audit = %#v", fields)
	}
}
