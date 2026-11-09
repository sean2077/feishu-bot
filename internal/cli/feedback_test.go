package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestParseFeedbackExport(t *testing.T) {
	o, err := Parse([]string{"feedback", "export"})
	if err != nil || o.Command != "feedback" || o.Subcommand != "export" || o.FeedbackValue != "useless" {
		t.Fatalf("feedback parse = %#v %v", o, err)
	}
	o, err = Parse([]string{"feedback", "export", "--value", "all", "--since", "7", "--project", "team", "--config", "config.json"})
	if err != nil || o.FeedbackValue != "all" || o.FeedbackSince != "7" || o.FeedbackProject != "team" {
		t.Fatalf("feedback options = %#v %v", o, err)
	}
	for _, args := range [][]string{{"feedback", "bogus"}, {"feedback", "export", "--value", "maybe"}, {"feedback", "export", "--value"}} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func exportFeedbackRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		lines = append(lines, value)
	}
	return lines
}

func writeFeedbackRows(t *testing.T, c core.Config, rows []struct {
	id, project, value, question string
	at                           time.Time
}) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := st.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, row.id, "room", row.project, "session", "native-"+row.id, "output-"+row.id, "message-"+row.id, "ou_"+row.id, row.value, row.question, "answer-"+row.id, row.at.UnixMilli()); err != nil {
			st.Close()
			t.Fatal(err)
		}
	}
	return st
}

func TestFeedbackExportFiltersAndJSONL(t *testing.T) {
	dir := t.TempDir()
	path := writeInstallConfig(t, dir)
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []struct {
		id, project, value, question string
		at                           time.Time
	}{
		{"useless", "p", "useless", "question useless", now.Add(-time.Hour)},
		{"useful", "p", "useful", "question useful", now.Add(-2 * time.Hour)},
		{"other", "other", "useless", "", now.Add(-3 * time.Hour)},
		{"old", "p", "useless", "old question", now.Add(-100 * 24 * time.Hour)},
	}
	st := writeFeedbackRows(t, c, rows)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []map[string]any {
		o, err := Parse(append([]string{"feedback", "export", "--config", path}, args...))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runFeedbackCommand(context.Background(), o, &out); err != nil {
			t.Fatal(err)
		}
		return exportFeedbackRows(t, writeTemp(t, out.Bytes()))
	}
	if got := run(); len(got) != 3 {
		t.Fatalf("default useless export = %#v", got)
	}
	for _, value := range run() {
		if value["value"] != "useless" {
			t.Fatalf("default value filter = %#v", value)
		}
	}
	if got := run("--value", "useful"); len(got) != 1 || got[0]["user"] != "ou_useful" {
		t.Fatalf("useful export = %#v", got)
	}
	if got := run("--value", "all"); len(got) != 4 {
		t.Fatalf("all export = %d", len(got))
	}
	if got := run("--project", "p"); len(got) != 2 {
		t.Fatalf("project export = %#v", got)
	}
	if got := run("--since", "1"); len(got) != 2 {
		t.Fatalf("since days export = %#v", got)
	}
	if got := run("--since", now.Add(-90*time.Minute).Format(time.RFC3339)); len(got) != 1 {
		t.Fatalf("since RFC3339 export = %#v", got)
	}
	got := run("--value", "all", "--project", "other")
	if len(got) != 1 || got[0]["questionMissing"] != true || got[0]["question"] != "" {
		t.Fatalf("question-missing export = %#v", got)
	}
	record := got[0]
	for _, key := range []string{"at", "project", "room", "session", "nativeSessionId", "value", "user", "question", "answerSummary", "messageId"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("export record missing %q: %#v", key, record)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, record["at"].(string)); err != nil {
		t.Fatalf("at is not RFC3339: %#v", record["at"])
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFeedbackExportIsReadOnlyAndConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := writeInstallConfig(t, dir)
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if _, err := st.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES('one','room','p','session',NULL,'output','message','ou_one','useless','question','answer',?)`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(c.DataDir, "gateway.sqlite")
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Parse([]string{"feedback", "export", "--config", path})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runFeedbackCommand(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("ou_one")) {
		t.Fatalf("concurrent export output = %s", out.String())
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("feedback export modified the database file")
	}
	if _, err := st.DB.Exec(`INSERT INTO feedback(id,room,project,session,native_session,output,message,user,value,question,answer_summary,at)
VALUES('two','room','p','session',NULL,'output2','message2','ou_two','useless','question2','answer2',?)`, now.Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runFeedbackCommand(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("ou_two")) {
		t.Fatalf("writer-visible export = %s", out.String())
	}
}
