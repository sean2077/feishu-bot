package feedbackexport_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/feedbackexport"
	"github.com/sean2077/feishu-bot/internal/store"
)

func records(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	return result
}

func TestWriteDailyArchiveIsExclusiveAndPrivate(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 11, 10, 23, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	path, err := feedbackexport.Write(dir, "team-tools", day, []feedbackexport.Record{
		{AtMS: 1, At: "1970-01-01T00:00:00Z", Project: "team-tools", Value: "useless", User: "ou_one", Question: "q1", AnswerSummary: "a1", MessageID: "m1"},
		{AtMS: 2, At: "1970-01-01T00:00:00Z", Project: "team-tools", Value: "useful", User: "ou_two", QuestionMissing: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "feedback-team-tools-2026-11-10.jsonl" {
		t.Fatalf("unexpected archive name: %s", path)
	}
	if rows := records(t, path); len(rows) != 2 || rows[0]["messageId"] != "m1" || rows[1]["questionMissing"] != true {
		t.Fatalf("archive records: %#v", rows)
	}
	if _, ok := records(t, path)[0]["id"]; ok {
		t.Fatal("cursor id must not be exported")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("archive mode = %v", info.Mode().Perm())
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = feedbackexport.Write(dir, "team-tools", day, []feedbackexport.Record{{Project: "team-tools", Value: "useless"}})
	if !errors.Is(err, feedbackexport.ErrExists) {
		t.Fatalf("second archive write = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("existing daily archive was overwritten")
	}
	if _, err := feedbackexport.Write(dir, "../escape", day, nil); err == nil {
		t.Fatal("path separator in project was accepted")
	}
}

func TestQueryCursorOrdersByTimeThenID(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, row := range []struct {
		id string
		at int64
	}{{"b", 2}, {"a", 2}, {"c", 1}, {"d", 3}} {
		if _, err := s.DB.Exec(`INSERT INTO feedback(id,room,project,session,output,message,user,value,question,answer_summary,at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, row.id, "room", "p", "session", "output-"+row.id, "message", "ou_"+row.id, "useless", "", "answer", row.at); err != nil {
			t.Fatal(err)
		}
	}
	all, err := feedbackexport.Query(context.Background(), s.DB, feedbackexport.Filter{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, record := range all {
		order = append(order, record.ID)
	}
	if strings.Join(order, ",") != "c,a,b,d" {
		t.Fatalf("query order = %v", order)
	}
	after, err := feedbackexport.Query(context.Background(), s.DB, feedbackexport.Filter{Project: "p", AfterAt: 2, AfterID: "a", HasAfter: true})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, record := range after {
		ids = append(ids, record.ID)
	}
	if strings.Join(ids, ",") != "b,d" {
		t.Fatalf("cursor query = %v", ids)
	}
}
