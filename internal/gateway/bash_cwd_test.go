package gateway

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestBashCwdPrefixRejectedAuditedWithoutCommand(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Projects[0].Bash.RejectCwdPrefix = true })
	f.send(f.message("one", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "bash_cwd_prefix_rejected"})
	eventually(t, func() bool {
		var count int
		_ = f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE room=? AND event='bash_cwd_prefix_rejected'", f.room().Key).Scan(&count)
		return count == 1
	})
	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM audit WHERE room=? AND event='bash_cwd_prefix_rejected'", f.room().Key).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["session"] != d.SessionID {
		t.Fatalf("audit session = %#v, want %q", fields, d.SessionID)
	}
	for _, forbidden := range []string{"command", "input", "toolUseId"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("audit leaked %s: %#v", forbidden, fields)
		}
	}
}
