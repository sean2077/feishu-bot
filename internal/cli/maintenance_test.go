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
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestParseMaintenanceAckUncertain(t *testing.T) {
	o, err := Parse([]string{"maintenance", "ack-uncertain", "--config", "cfg.json", "--before", "2026-11-16T08:00:00Z", "--apply", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC).UnixMilli()
	if !o.MaintenanceBeforeSet || o.MaintenanceBefore != want || !o.Apply || !o.JSON || o.Subcommand != "ack-uncertain" || !o.ConfigExplicit {
		t.Fatalf("parsed options = %#v", o)
	}
	o, err = Parse([]string{"maintenance", "ack-uncertain", "--before=1763270400123"})
	if err != nil || o.MaintenanceBefore != 1763270400123 || o.Apply {
		t.Fatalf("unix millis options = %#v %v", o, err)
	}
	for _, args := range [][]string{
		{"maintenance", "ack-uncertain"},
		{"maintenance", "ack-uncertain", "--before"},
		{"maintenance", "ack-uncertain", "--before", "not-a-time"},
		{"maintenance", "ack-uncertain", "--before", "-1"},
		{"maintenance", "ack-uncertain", "--before", "1", "--before", "2"},
		{"maintenance", "ack-uncertain", "--before", "1", "--apply", "--apply"},
		{"maintenance", "ack-uncertain", "--before", "1", "--unknown"},
		{"maintenance", "unknown"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	o, err = Parse([]string{"maintenance", "--help"})
	if err != nil || o.Command != "help" || o.Subcommand != "maintenance" {
		t.Fatalf("maintenance help = %#v %v", o, err)
	}
}

func TestMaintenanceDryRunDoesNotWriteAndApplyMarksBefore(t *testing.T) {
	dir := t.TempDir()
	configPath := writeInstallConfig(t, dir)
	c, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, query := range []string{
		`INSERT INTO inbox(room,id,message,status,at) VALUES('room-a','om_old','{"text":"private input"}','uncertain',100)`,
		`INSERT INTO inbox(room,id,message,status,at) VALUES('room-b','om_new','{}','uncertain',300)`,
		`INSERT INTO inbox(room,id,message,status,at) VALUES('room-a','om_done','{}','done',100)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-uncertain','room-a','s','private output',0,'uncertain',100)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-blocked','room-b','s','private output',0,'blocked',100)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-new','room-a','s','',0,'uncertain',300)`,
		`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-pending','room-a','s','',0,'pending',100)`,
	} {
		if _, err := st.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	dry, err := Parse([]string{"maintenance", "ack-uncertain", "--config", configPath, "--before", "200", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runMaintenance(context.Background(), c, dry, &out); err != nil {
		t.Fatal(err)
	}
	var report ackUncertainReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err, out.String())
	}
	if report.Mode != "dry-run" || report.Candidates == nil || report.Updated != nil {
		t.Fatalf("dry-run report = %#v", report)
	}
	if report.Candidates.Inbox.Count != 1 || report.Candidates.Outbox.Count != 2 {
		t.Fatalf("dry-run candidates = %#v", report.Candidates)
	}
	if strings.Contains(out.String(), "private") {
		t.Fatalf("dry-run exposed record bodies: %s", out.String())
	}
	for query, want := range map[string]string{
		`SELECT status FROM inbox WHERE id='om_old'`:        "uncertain",
		`SELECT state FROM outbox WHERE id='out-uncertain'`: "uncertain",
		`SELECT state FROM outbox WHERE id='out-blocked'`:   "blocked",
	} {
		var got string
		if err := st.DB.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("dry-run changed %s = %q (%v)", query, got, err)
		}
	}

	apply, err := Parse([]string{"maintenance", "ack-uncertain", "--config", configPath, "--before", "200", "--apply", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runMaintenance(context.Background(), c, apply, &out); err != nil {
		t.Fatal(err)
	}
	report = ackUncertainReport{}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err, out.String())
	}
	if report.Mode != "apply" || report.Updated == nil || report.Updated.Inbox != 1 || report.Updated.Outbox != 2 {
		t.Fatalf("apply report = %#v", report)
	}
	for query, want := range map[string]string{
		`SELECT status FROM inbox WHERE id='om_old'`:        "acknowledged",
		`SELECT status FROM inbox WHERE id='om_new'`:        "uncertain",
		`SELECT state FROM outbox WHERE id='out-uncertain'`: "acknowledged",
		`SELECT state FROM outbox WHERE id='out-blocked'`:   "acknowledged",
		`SELECT state FROM outbox WHERE id='out-new'`:       "uncertain",
		`SELECT state FROM outbox WHERE id='out-pending'`:   "pending",
	} {
		var got string
		if err := st.DB.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("apply %s = %q (%v), want %q", query, got, err, want)
		}
	}
	human, err := Parse([]string{"maintenance", "ack-uncertain", "--config", configPath, "--before", "200", "--apply"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runMaintenance(context.Background(), c, human, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "已更新为 acknowledged：inbox 0 条，outbox 0 条") {
		t.Fatalf("human apply report = %s", out.String())
	}
}

func TestMaintenanceCommandErrorsAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	configPath := writeInstallConfig(t, dir)
	c, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB.Exec(`INSERT INTO inbox(room,id,message,status,at) VALUES('room','om_old','{}','uncertain',100)`); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run(context.Background(), []string{"maintenance", "ack-uncertain", "--config", configPath, "--before", "200"}, &out, &errs); code != 0 {
		t.Fatalf("dry-run exit = %d stderr=%s", code, errs.String())
	}
	if !strings.Contains(out.String(), "dry-run") || !strings.Contains(out.String(), "未写入数据库") {
		t.Fatalf("dry-run output = %s", out.String())
	}
	out.Reset()
	errs.Reset()
	if code := Run(context.Background(), []string{"maintenance", "ack-uncertain", "--config", configPath}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "requires --before") {
		t.Fatalf("missing before exit = %d stderr=%s", code, errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := Run(context.Background(), []string{"maintenance", "ack-uncertain", "--config", configPath, "--before", "yesterday"}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "RFC3339 or Unix milliseconds") {
		t.Fatalf("invalid before exit = %d stderr=%s", code, errs.String())
	}
	out.Reset()
	if code := Run(context.Background(), []string{"maintenance", "--help"}, &out, &errs); code != 0 || !strings.Contains(out.String(), "maintenance ack-uncertain") {
		t.Fatalf("help exit = %d stdout=%s stderr=%s", code, out.String(), errs.String())
	}
}

func TestDoctorReportsPendingUncertainReadOnly(t *testing.T) {
	dir := t.TempDir()
	c := config.Defaults()
	c.DataDir = dir
	c.Model.BaseURL = "https://model.example"
	c.Runtime.ClaudeExecutable = filepath.Join(dir, "missing-claude")
	path := filepath.Join(dir, "gateway.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB.Exec(`INSERT INTO inbox(room,id,message,status,at) VALUES('room-a','om_old','{"text":"private"}','uncertain',100),('room-b','om_new','{}','uncertain',300)`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB.Exec(`INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('out-stuck','room-a','s','private',0,'blocked',200)`); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report := RunDoctor(context.Background(), c, false, false)
	if report.PendingUncertain.Status != "observed" || report.PendingUncertain.Inbox.Count != 2 || report.PendingUncertain.Outbox.Count != 1 {
		t.Fatalf("pending uncertain report = %#v", report.PendingUncertain)
	}
	found := false
	for _, check := range report.Checks {
		if check.Name == "pending uncertain records" {
			found = true
			if !check.OK || !strings.Contains(check.Detail, "2 uncertain inbox") {
				t.Fatalf("pending uncertain check = %#v", check)
			}
		}
	}
	if !found {
		t.Fatal("doctor omitted pending uncertain check")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("doctor modified the production database")
	}
	if strings.Contains(report.PendingUncertain.Inbox.Samples[0], "private") {
		t.Fatal("doctor exposed input body")
	}
}
