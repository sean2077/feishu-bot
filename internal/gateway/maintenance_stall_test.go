package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func maintenanceStallAudits(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='maintenance_stalled'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMaintenanceStallIsAuditedOnlyAfterConsecutiveNoProgress(t *testing.T) {
	f := newFixture(t, nil)
	f.g.noteMaintenance(true, false)
	f.g.noteMaintenance(true, false)
	if got := maintenanceStallAudits(t, f); got != 0 {
		t.Fatalf("stall audited before the threshold: %d", got)
	}
	f.g.noteMaintenance(true, false)
	eventually(t, func() bool { return maintenanceStallAudits(t, f) == 1 })
	f.g.noteMaintenance(true, false)
	if got := maintenanceStallAudits(t, f); got != 1 {
		t.Fatalf("continued stall posted %d audits, want one episode", got)
	}
	f.g.noteMaintenance(true, true)
	f.g.noteMaintenance(true, false)
	f.g.noteMaintenance(true, false)
	f.g.noteMaintenance(true, false)
	eventually(t, func() bool { return maintenanceStallAudits(t, f) == 2 })
}

func TestStatusReportsLastFullMaintenance(t *testing.T) {
	f := newFixture(t, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		report, err := f.s.Maintain(context.Background(), time.Now().UnixMilli(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Deadline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance never completed a full sweep")
		}
	}
	text, handled, err := f.g.management("/status", "--json", f.room(), "member")
	if err != nil || !handled {
		t.Fatalf("status = handled %t err %v", handled, err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	maintenance, _ := value["maintenance"].(map[string]any)
	at, _ := maintenance["lastFullAt"].(string)
	if maintenance == nil || strings.TrimSpace(at) == "" {
		t.Fatalf("status maintenance metric = %#v", value["maintenance"])
	}
	plain, _, err := f.g.management("/status", "", f.room(), "member")
	if err != nil || !strings.Contains(plain, "维护：最近完整清理") {
		t.Fatalf("plain status missing maintenance line: %q %v", plain, err)
	}
}
