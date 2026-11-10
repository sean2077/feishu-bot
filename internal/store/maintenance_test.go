package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func maintenancePolicyNamed(t *testing.T, name string, now int64) maintenancePolicy {
	t.Helper()
	for _, p := range maintenancePolicies(now, 1) {
		if p.name == name {
			return p
		}
	}
	t.Fatal("missing maintenance policy", name)
	return maintenancePolicy{}
}

// A bounded maintenance call need not visit every table before its budget
// expires. Require committed progress across calls before checking retention.
func maintainAllPolicies(t *testing.T, s *Store, now int64) {
	t.Helper()
	policies := maintenancePolicies(now, 1)
	pending := map[string]bool{}
	for _, policy := range policies {
		pending[policy.name] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var lastReport MaintenanceReport
	var lastErr error
	for round := 0; round < 2*len(policies) && ctx.Err() == nil; round++ {
		lastReport, lastErr = s.Maintain(ctx, now, 1)
		if lastErr != nil && !errors.Is(lastErr, context.DeadlineExceeded) {
			t.Fatalf("maintenance round %d: %+v: %v", round+1, lastReport, lastErr)
		}
		for _, entry := range lastReport.Policies {
			if entry.Error == "" && !entry.Deadline {
				delete(pending, entry.Name)
			}
		}
		if len(pending) == 0 {
			return
		}
	}
	t.Fatalf("maintenance did not commit every policy: pending=%v context=%v report=%+v error=%v", pending, ctx.Err(), lastReport, lastErr)
}

func TestMaintenanceBudgetsAndPersistentProgress(t *testing.T) {
	s := openTest(t)
	if err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		for i := 0; i < maintenanceChanges*3; i++ {
			if _, err := tx.Exec("INSERT INTO audit(at,room,event,detail) VALUES(1,'r','old','{}')"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := maintenancePolicyNamed(t, "audit", 100*dayMS)
	for i := 0; i < 3; i++ {
		r, err := s.maintainPolicy(context.Background(), p, 0)
		if err != nil || r.Changed != maintenanceChanges || r.Candidates > maintenanceCandidates {
			t.Fatalf("batch %d: %+v %v", i, r, err)
		}
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM audit").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	r, err := s.maintainPolicy(context.Background(), p, 0)
	if err != nil || r.Candidates != 0 {
		t.Fatal(r, err)
	}
	if err = s.DB.QueryRow("SELECT count(*) FROM maintenance_cursors WHERE name='audit'").Scan(&n); err != nil || n != 0 {
		t.Fatal("completed cursor did not reset", n, err)
	}
}

func TestMaintenanceProtectedRowsCannotStarveLaterRows(t *testing.T) {
	s := openTest(t)
	if err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		for i := 0; i < maintenanceCandidates+1; i++ {
			id := fmt.Sprintf("old-%04d", i)
			if _, err := tx.Exec("INSERT INTO outputs(id,room,session,at) VALUES(?,'r','s',1)", id); err != nil {
				return err
			}
			if i < maintenanceCandidates {
				if _, err := tx.Exec("INSERT INTO outbox(id,room,session,text,target,success,state,at) VALUES(?,'r','s','evidence',?,0,'uncertain',1)", id, id); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := maintenancePolicyNamed(t, "outputs", 40*dayMS)
	r, err := s.maintainPolicy(context.Background(), p, 0)
	if err != nil || r.Candidates != maintenanceCandidates || r.Changed != 0 {
		t.Fatal(r, err)
	}
	// Simulate another Store after restart: no in-memory cursor is required.
	restarted := &Store{DB: s.DB}
	r, err = restarted.maintainPolicy(context.Background(), p, 0)
	if err != nil || r.Changed != 1 {
		t.Fatal("later unreferenced output starved", r, err)
	}
	var n int
	if err = s.DB.QueryRow("SELECT count(*) FROM outputs").Scan(&n); err != nil || n != maintenanceCandidates {
		t.Fatal(n, err)
	}
}

func TestMaintenanceRetainsUncertainEvidenceAndUsageDedup(t *testing.T) {
	s := openTest(t)
	queries := []string{
		`INSERT INTO inbox(room,id,message,status,at,session) VALUES('r','unknown','{"id":"unknown"}','uncertain',1,'s')`,
		`INSERT INTO inbox(room,id,message,status,at,session) VALUES('r','related','{"id":"related"}','done',1,'s')`,
		`INSERT INTO action_runs(id,room,session,action,digest,state,at) VALUES('unknown','r','s','write','hash','uncertain',1)`,
		`INSERT INTO action_runs(id,room,session,action,digest,state,at) VALUES('done','r','finished','write','hash','done',1)`,
		`INSERT INTO requests(id,room,session,kind,digest,state,expires) VALUES('pending','r','other','permission','hash','pending',1)`,
		`INSERT INTO inbox(room,id,message,status,at,session) VALUES('r','waiting','{"id":"waiting"}','done',1,'other')`,
		`INSERT INTO requests(id,room,session,kind,digest,state,expires) VALUES('expired','r','finished','permission','hash','expired',1)`,
		`INSERT INTO usage_results(session,result_id,room,at,kind,success,models) VALUES('s','result','r',1,'native',1,'[]')`,
	}
	for _, q := range queries {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	maintainAllPolicies(t, s, 200*dayMS)
	for _, q := range []string{
		`SELECT count(*) FROM inbox WHERE id='unknown' AND message!='{}'`,
		`SELECT count(*) FROM action_runs WHERE id='unknown' AND state='uncertain'`,
		`SELECT count(*) FROM inbox WHERE id='waiting' AND message!='{}'`,
		`SELECT count(*) FROM requests WHERE id='pending'`,
		`SELECT count(*) FROM usage_results`,
	} {
		var n int
		if err := s.DB.QueryRow(q).Scan(&n); err != nil || n != 1 {
			t.Fatal(q, n, err)
		}
	}
	// Uncertain evidence keeps only its own row; actual pending work still
	// protects its session. Terminal rows beside the uncertain one age out.
	for _, q := range []string{`SELECT count(*) FROM action_runs WHERE id='done'`, `SELECT count(*) FROM requests WHERE id='expired'`, `SELECT count(*) FROM inbox WHERE id='related'`} {
		var n int
		if err := s.DB.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Fatal(q, n, err)
		}
	}
}

func TestMaintenanceDoesNotRewriteEmptyBodies(t *testing.T) {
	s := openTest(t)
	if _, err := s.DB.Exec(`INSERT INTO inbox(room,id,message,status,at) VALUES('r','empty','{}','done',1),('r','full','{"id":"full"}','done',1)`); err != nil {
		t.Fatal(err)
	}
	p := maintenancePolicyNamed(t, "inbox_bodies", 2*dayMS)
	r, err := s.maintainPolicy(context.Background(), p, 0)
	if err != nil || r.Candidates != 1 || r.Changed != 1 {
		t.Fatal(r, err)
	}
	for i := 0; i < 2; i++ {
		r, err = s.maintainPolicy(context.Background(), p, 0)
		if err != nil || r.Changed != 0 || r.Candidates != 0 {
			t.Fatal("empty body revisited", r, err)
		}
	}
}

func TestMaintenanceCancellationDoesNotAdvanceCursor(t *testing.T) {
	s := openTest(t)
	if _, err := s.DB.Exec("INSERT INTO audit(at,room,event,detail) VALUES(1,'r','old','{}')"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := s.Maintain(ctx, 200*dayMS, 1)
	if !errors.Is(err, context.Canceled) || r.Changed != 0 {
		t.Fatal(r, err)
	}
	var n int
	if err = s.DB.QueryRow("SELECT count(*) FROM maintenance_cursors").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestMaintenanceAgeScanUsesIndexWithoutSort(t *testing.T) {
	s := openTest(t)
	for _, p := range maintenancePolicies(time.Now().UnixMilli(), 1) {
		q := "EXPLAIN QUERY PLAN SELECT rowid," + p.at + " FROM " + p.table + " WHERE (" + p.eligible + ") AND " + p.at + "<? AND (" + p.at + ",rowid)>(?,?) ORDER BY " + p.at + ",rowid LIMIT ?"
		rows, err := s.DB.Query(q, int64(1<<62), int64(0), int64(0), maintenanceCandidates)
		if err != nil {
			t.Fatal(p.name, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "INDEX") || strings.Contains(plan.String(), "TEMP B-TREE") {
			t.Fatalf("%s unbounded/sorted candidate scan: %s", p.name, plan.String())
		}
	}
}

func TestTableStatsRoomScopeAndUnknownUsageRetention(t *testing.T) {
	s := openTest(t)
	if _, err := s.DB.Exec(`INSERT INTO usage_results(session,result_id,room,at,kind,success,models) VALUES('s','a','r',42,'native',1,'[]'),('x','b','other',1,'native',1,'[]')`); err != nil {
		t.Fatal(err)
	}
	stats, err := s.TableStats(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range stats {
		if v.Table == "usage_results" {
			found = true
			if v.Count != 1 || v.OldestAt == nil || *v.OldestAt != 42 || !strings.Contains(v.Retention, "deduplication") {
				t.Fatal(v)
			}
		}
	}
	if !found {
		t.Fatal("usage results not diagnosed")
	}
}

func TestMaintenancePreservesSessionlessContextDuringActiveWork(t *testing.T) {
	s := openTest(t)
	for _, q := range []string{
		`INSERT INTO inbox(room,id,message,status,at) VALUES('active','background','{"id":"background","resources":[{"kind":"file","key":"source"}]}','context',1)`,
		`INSERT INTO context_refs(room,id,message,at) VALUES('active','quoted','{"id":"quoted","resources":[{"kind":"file","key":"quote"}]}',1)`,
		`INSERT INTO inbox(room,id,message,status,at,session) VALUES('active','running','{}','consumed',1,'session')`,
		`INSERT INTO context_refs(room,id,message,at) VALUES('idle','expired','{}',1)`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	maintainAllPolicies(t, s, 2*dayMS)
	for _, id := range []string{"background", "quoted"} {
		m, err := s.ContextMessage("active", id)
		if err != nil || m == nil || len(m.Resources) != 1 {
			t.Fatal("active attachment metadata was evicted", id, m, err)
		}
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM context_refs WHERE room='idle'").Scan(&n); err != nil || n != 0 {
		t.Fatal("inactive context did not expire", n, err)
	}
}

func TestMaintenanceKeepsDiscardedOriginDedupWithoutItsBody(t *testing.T) {
	s := openTest(t)
	if _, err := s.DB.Exec(`INSERT INTO inbox(room,id,message,status,at,session) VALUES('r','discard-source','{"id":"discard-source"}','done',1,'s');
INSERT INTO output_discarded_origins(room,session,origin,output,actor,at) VALUES('r','s','discard-source','discarded','owner',1);`); err != nil {
		t.Fatal(err)
	}
	maintainAllPolicies(t, s, 100*dayMS)
	seen, err := s.Seen("r", "discard-source")
	if err != nil || !seen {
		t.Fatal("discard origin became eligible for native input replay", seen, err)
	}
	var body string
	if err = s.DB.QueryRow("SELECT message FROM inbox WHERE room='r' AND id='discard-source'").Scan(&body); err != nil || body != "{}" {
		t.Fatal("dedup marker retained the unnecessary message body", body, err)
	}
}

func TestMaintenanceDeadlineIsResumableAndRecordsFullCompletion(t *testing.T) {
	s := openTest(t)
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	report, err := s.Maintain(expired, time.Now().UnixMilli(), 1)
	if err != nil || !report.Deadline || report.Progress {
		t.Fatalf("deadline report = %+v, %v", report, err)
	}
	maintainAllPolicies(t, s, time.Now().UnixMilli())
	at, ok, err := s.LastFullMaintenance(context.Background())
	if err != nil || !ok || at <= 0 {
		t.Fatalf("last full maintenance = %d %t %v", at, ok, err)
	}
}
