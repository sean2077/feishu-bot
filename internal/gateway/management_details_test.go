package gateway

import (
	"strings"
	"testing"
)

func TestManagementDiagnosticsStayRoomScopedAndRespectRole(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	other, _ := RoomFor(f.c, "other", "")
	otherSession, _ := f.s.Session(other.Key)
	for _, row := range []struct{ message, room, snapshot string }{
		{"card-here", r.Key, `{"a":"中"}`},
		{"card-elsewhere", other.Key, strings.Repeat("private", 100)},
	} {
		if _, err := f.s.DB.Exec("INSERT INTO feishu_cards(message,card,room,snapshot) VALUES(?,'card-id',?,?)", row.message, row.room, row.snapshot); err != nil { t.Fatal(err) }
	}
	rate, err := f.g.saveOutput(f.s.DB, r, s.ID, "private reply body", true, "", "", "", nil, nil, "", "", "reply", "pending")
	if err != nil { t.Fatal(err) }
	if _, err := f.s.DB.Exec("UPDATE outbox SET delivery_error=?,retry_count=2,retry_after=123456 WHERE id=?", "feishu: message send failed (HTTP 429, code 99991400) request_id=trace_SAFE Bearer fixture-credential", rate); err != nil { t.Fatal(err) }
	if _, err := f.g.saveOutput(f.s.DB, r, s.ID, "", true, "", "", "", nil, nil, "report.md", "frozen", "reply", "blocked"); err != nil { t.Fatal(err) }
	if _, err := f.g.saveOutput(f.s.DB, other, otherSession.ID, "", true, "", "", "", nil, nil, "", "", "reply", "uncertain"); err != nil { t.Fatal(err) }
	view, err := f.g.statusView(r)
	if err != nil { t.Fatal(err) }
	snapshots := argsOf(view["cardSnapshots"])
	states := argsOf(view["outboxStates"])
	if snapshots["count"] != int64(1) || snapshots["bytes"] != int64(11) || states["pending"] != int64(1) || states["blocked"] != int64(1) || states["uncertain"] != nil {
		t.Fatal("status diagnostics counted another room", view)
	}
	owner, _, err := f.g.management("/outputs", "--json", r, "owner")
	if err != nil { t.Fatal(err) }
	member, _, err := f.g.management("/outputs", "--json", r, "member")
	if err != nil { t.Fatal(err) }
	if !strings.Contains(owner, "trace_SAFE") || !strings.Contains(owner, "rate_limited") || !strings.Contains(owner, `"retryCount": 2`) || !strings.Contains(owner, `"filePending": true`) {
		t.Fatal("maintainer diagnostics lost safe cause / attempt metadata", owner)
	}
	if strings.Contains(owner, "fixture-credential") || strings.Contains(owner, "private reply body") || strings.Contains(member, "trace_SAFE") || strings.Contains(member, "fixture-credential") || f.starts.Load() != 0 {
		t.Fatal("management leaked private details or started model", owner, member)
	}
}
