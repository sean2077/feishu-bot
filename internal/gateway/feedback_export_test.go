package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func writeFeedbackTestRow(t *testing.T, s *store.Store, id, project string, at int64) {
	t.Helper()
	if _, err := s.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, "room", project, "session", "native-"+id, "output-"+id, "message-"+id, "ou_"+id, "useless", "question-"+id, "answer-"+id, at); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledFeedbackExportWritesOneFilePerProjectAndDay(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, func(c *core.Config) {
		c.Feedback.Export = core.FeedbackExportConfig{Enabled: true, Dir: dir}
	})
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	writeFeedbackTestRow(t, f.s, "old", "p1", now.Add(-100*24*time.Hour).UnixMilli())
	writeFeedbackTestRow(t, f.s, "one", "p1", now.UnixMilli())
	writeFeedbackTestRow(t, f.s, "two", "p2", now.Add(time.Second).UnixMilli())
	if paused := f.g.exportFeedback(context.Background(), now.UnixMilli()); paused != nil {
		t.Fatalf("export paused: %v", paused)
	}
	for project, count := range map[string]int{"p1": 2, "p2": 1} {
		path := filepath.Join(dir, "feedback-"+project+"-2026-11-10.jsonl")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(nonemptyLines(data)); got != count {
			t.Fatalf("%s rows = %d, want %d: %s", project, got, count, data)
		}
		mark, ok, err := f.s.FeedbackExportMark(context.Background(), project)
		if err != nil || !ok || mark.File != path || mark.Rows != int64(count) {
			t.Fatalf("%s mark = %+v %t %v", project, mark, ok, err)
		}
	}
	rows, err := f.s.DB.Query("SELECT detail FROM audit WHERE event='feedback_exported' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]map[string]any{}
	for rows.Next() {
		var detail string
		if err = rows.Scan(&detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var fields map[string]any
		if err = json.Unmarshal([]byte(detail), &fields); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		exported[fmt.Sprint(fields["project"])] = fields
	}
	rows.Close()
	if len(exported) != 2 {
		t.Fatalf("feedback export audits = %#v", exported)
	}
	for project, count := range map[string]int64{"p1": 2, "p2": 1} {
		fields := exported[project]
		if fields["date"] != "2026-11-10" || int64(fields["rows"].(float64)) != count || fields["file"] != "feedback-"+project+"-2026-11-10.jsonl" {
			t.Fatalf("%s export audit = %#v", project, fields)
		}
		for _, forbidden := range []string{"question", "answer", "answerSummary", "user", "open_id", "message"} {
			if _, ok := fields[forbidden]; ok {
				t.Fatalf("%s export audit leaked %s: %#v", project, forbidden, fields)
			}
		}
	}
	firstP1, err := os.ReadFile(filepath.Join(dir, "feedback-p1-2026-11-10.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// A later same-day row is not written into the existing file. It waits for
	// the next daily archive and the file marker prevents an overwrite.
	writeFeedbackTestRow(t, f.s, "late", "p1", now.Add(time.Hour).UnixMilli())
	if paused := f.g.exportFeedback(context.Background(), now.Add(time.Hour).UnixMilli()); paused != nil {
		t.Fatalf("same-day export paused: %v", paused)
	}
	again, err := os.ReadFile(filepath.Join(dir, "feedback-p1-2026-11-10.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(firstP1) != string(again) {
		t.Fatal("same-day archive was overwritten")
	}
	next := now.Add(24 * time.Hour)
	if paused := f.g.exportFeedback(context.Background(), next.UnixMilli()); paused != nil {
		t.Fatalf("next-day export paused: %v", paused)
	}
	data, err := os.ReadFile(filepath.Join(dir, "feedback-p1-2026-11-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(nonemptyLines(data)); got != 1 {
		t.Fatalf("next-day rows = %d, want 1: %s", got, data)
	}
}

func nonemptyLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func TestFeedbackExportErrorKeyIgnoresTemporaryPaths(t *testing.T) {
	first := &os.PathError{Op: "open", Path: filepath.Join("tmp", ".feedback-p-2026-11-10.jsonl.tmp-1"), Err: os.ErrPermission}
	second := &os.PathError{Op: "open", Path: filepath.Join("tmp", ".feedback-p-2026-11-10.jsonl.tmp-2"), Err: os.ErrPermission}
	key := feedbackExportErrorKey(first)
	if key != feedbackExportErrorKey(second) {
		t.Fatalf("temporary path changed the audit key: %q vs %q", key, feedbackExportErrorKey(second))
	}
	if strings.Contains(key, "tmp-") {
		t.Fatalf("audit key contains a temporary name: %q", key)
	}
}

func TestScheduledFeedbackExportFailurePausesRetentionAndAuditsOnce(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(c *core.Config) {
		c.Feedback.Export = core.FeedbackExportConfig{Enabled: true, Dir: file}
	})
	now := time.Now().UnixMilli()
	writeFeedbackTestRow(t, f.s, "old", "project", 1)
	if _, err := f.s.DB.Exec("INSERT INTO audit(at,room,event,detail) VALUES(1,'room','old','{}')"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		f.g.Sweep(ctx, now)
		var auditRows int
		if err := f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='old'").Scan(&auditRows); err != nil {
			t.Fatal(err)
		}
		if auditRows == 0 {
			break
		}
	}
	var feedbackRows, failures int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM feedback WHERE id='old'").Scan(&feedbackRows); err != nil {
		t.Fatal(err)
	}
	if feedbackRows != 1 {
		t.Fatal("failed export did not pause feedback retention")
	}
	if err := f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='feedback_export_failed'").Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("export failures audited %d times", failures)
	}
	if _, ok, err := f.s.LastFullMaintenance(ctx); err != nil || ok {
		t.Fatalf("paused maintenance recorded a full pass: %t %v", ok, err)
	}

	// Recover in place: the next successful archive unblocks retention.
	dir := t.TempDir()
	f.g.config.Feedback.Export.Dir = dir
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		f.g.Sweep(ctx, now)
		if err := f.s.DB.QueryRow("SELECT count(*) FROM feedback WHERE id='old'").Scan(&feedbackRows); err != nil {
			t.Fatal(err)
		}
		if feedbackRows == 0 {
			break
		}
	}
	if feedbackRows != 0 {
		t.Fatal("retention did not resume after successful export")
	}
	path := filepath.Join(dir, "feedback-project-"+time.UnixMilli(now).UTC().Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("successful archive missing: %v", err)
	}
	if got := len(nonemptyLines(data)); got != 1 {
		t.Fatalf("archive rows = %d", got)
	}
}
