package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

const historyChat = `["tenant","app","project","group","oc_history",""]`

func historyTopic(root string) string {
	return strings.TrimSuffix(historyChat, "]") + `,"` + root + `"]`
}

// Use the production Go SQLite driver, schema, WAL and single-connection
// settings. Most rows are historical; only a small, fixed input set is live.
func seedInboxHistory(tb testing.TB, historyRows int) *Store {
	tb.Helper()
	s, err := Open(filepath.Join(tb.TempDir(), "history.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	rooms := []string{historyChat, historyTopic("om_a"), historyTopic("om_b"), historyTopic("om_c")}
	err = s.Transaction(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO inbox(room,id,message,status,at,session)
SELECT CASE i%4 WHEN 0 THEN ? WHEN 1 THEN ? WHEN 2 THEN ? ELSE ? END,
printf('past_%d',i),'{"text":"Archived discussion with a completed reply"}',
CASE i%10 WHEN 0 THEN 'uncertain' WHEN 1 THEN 'cancelled' ELSE 'done' END,i,'historical-session' FROM n`, historyRows, rooms[0], rooms[1], rooms[2], rooms[3])
		if err != nil {
			return err
		}
		for ri, room := range rooms {
			for _, status := range []string{"queued", "sent", "consumed"} {
				for n := 0; n < 2; n++ {
					id := fmt.Sprintf("active_%d_%s_%d", ri, status, n)
					d := core.Delivery{UUID: id, RoomKey: room, SessionID: fmt.Sprintf("session_%d", ri), Message: core.Message{ID: id, ChatID: "oc_history", SenderID: "ou_member", Text: "Current input", Mentioned: true}}
					payload, err := json.Marshal(d)
					if err != nil {
						return err
					}
					if _, err = tx.Exec("INSERT INTO inbox(room,id,message,status,at,uuid,session,payload) VALUES(?,?,'{}',?,?,?,?,?)", room, id, status, historyRows+n, id, d.SessionID, string(payload)); err != nil {
						return err
					}
				}
			}
		}
		_, err = tx.Exec(`INSERT INTO inbox(room,id,message,status,at) VALUES('["tenant","app","project","group","oc_other",""]','foreign','{}','queued',1)`)
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

func inboxQueryPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.DB.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plans, "\n")
}

func TestInboxQueriesUseLiveIndexesWithLargeHistory(t *testing.T) {
	s := seedInboxHistory(t, 10000)
	if n, err := s.CountPendingChat(historyChat); err != nil || n != 24 {
		t.Fatalf("chat count includes history/another chat: %d %v", n, err)
	}
	rooms, err := s.PendingRooms()
	if err != nil || len(rooms) != 5 {
		t.Fatalf("queued room discovery: %v %v", rooms, err)
	}
	pending, err := s.Pending(historyTopic("om_a"))
	if err != nil || len(pending) != 2 || pending[0].Message.ID != "active_1_queued_0" || pending[1].Message.ID != "active_1_queued_1" {
		t.Fatalf("queued order changed: %+v %v", pending, err)
	}
	countSQL, args := pendingChatQuery(historyChat)
	for _, tc := range []struct {
		name, query, index string
		args               []any
	}{
		{"chat count", countSQL, "inbox_active_room", args},
		{"queued rooms", "SELECT DISTINCT room FROM inbox WHERE status='queued'", "inbox_queued_room", nil},
		{"ordered input", "SELECT payload FROM inbox WHERE room=? AND status='queued' ORDER BY rowid", "inbox_queued_room", []any{historyTopic("om_a")}},
	} {
		plan := inboxQueryPlan(t, s, tc.query, tc.args...)
		t.Logf("%s:\n%s", tc.name, plan)
		if !strings.Contains(plan, tc.index) || strings.Contains(plan, "USE TEMP B-TREE") {
			t.Errorf("%s scans/sorts history instead of its live index:\n%s", tc.name, plan)
		}
	}
}

// BenchmarkInboxHistory compares current indexes to the prior schema on a
// private fixture. It measures local history size, not Feishu network latency.
func BenchmarkInboxHistory(b *testing.B) {
	for _, historyRows := range []int{1000, 100000} {
		for _, indexed := range []bool{false, true} {
			name := "previous_indexes"
			if indexed {
				name = "live_indexes"
			}
			b.Run(fmt.Sprintf("history_%d/%s", historyRows, name), func(b *testing.B) {
				s := seedInboxHistory(b, historyRows)
				if !indexed {
					if _, err := s.DB.Exec("DROP INDEX inbox_active_room; DROP INDEX inbox_queued_room"); err != nil {
						b.Fatal(err)
					}
				}
				b.Run("CountPendingChat", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if n, err := s.CountPendingChat(historyChat); err != nil || n != 24 {
							b.Fatal(n, err)
						}
					}
				})
				b.Run("PendingRooms", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if rooms, err := s.PendingRooms(); err != nil || len(rooms) != 5 {
							b.Fatal(rooms, err)
						}
					}
				})
				b.Run("PendingTopic", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if pending, err := s.Pending(historyTopic("om_a")); err != nil || len(pending) != 2 {
							b.Fatal(pending, err)
						}
					}
				})
			})
		}
	}
}
