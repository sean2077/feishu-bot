package gateway

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestProjectPromptUnavailableAuditedWithoutContent(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "hello", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "project_prompt_unavailable", Reason: "missing"})
	eventually(t, func() bool {
		var count int
		_ = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='project_prompt_unavailable'", f.room().Key).Scan(&count)
		return count == 1
	})
	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE room=? AND event='project_prompt_unavailable'", f.room().Key).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["project"] != "project" || fields["reason"] != "missing" {
		t.Fatalf("audit = %#v", fields)
	}
	for _, forbidden := range []string{"prompt", "content", "text"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("audit leaked %s: %#v", forbidden, fields)
		}
	}
}
