package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageDiagnosticsOnlyReadMetadata(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"gateway.sqlite": "not a SQLite database", "gateway.sqlite-wal": "wal", "gateway.sqlite-shm": "shared"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	exports := filepath.Join(dir, "exports")
	if err := os.Mkdir(exports, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exports, "answer.md"), []byte("中文"), 0600); err != nil {
		t.Fatal(err)
	}
	r := inspectStorage(context.Background(), dir)
	if r.Database.Status != "observed" || r.Database.Bytes != 21 || r.WAL.Bytes != 3 || r.SHM.Bytes != 6 || r.Exports.Bytes != 6 || r.Exports.Files != 1 {
		t.Fatalf("incorrect metadata footprint: %#v", r)
	}
	data, err := os.ReadFile(filepath.Join(dir, "gateway.sqlite"))
	if err != nil || string(data) != "not a SQLite database" {
		t.Fatal("storage diagnostic opened or rewrote the deployment database", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 4 {
		t.Fatal("storage diagnostic created state", err)
	}
}

func TestStorageDiagnosticDoesNotFollowExportSymlinks(t *testing.T) {
	dir := t.TempDir()
	exports := filepath.Join(dir, "exports")
	if err := os.Mkdir(exports, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("must not be included"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(exports, "link")); err != nil {
		t.Skip("symlink creation is unavailable on this host")
	}
	r := inspectStorage(context.Background(), dir)
	if r.Exports.Status != "partial" || r.Exports.Bytes != 0 || r.Exports.Files != 0 || r.Exports.Skipped != 1 {
		t.Fatalf("followed symlink or hid partial scan: %#v", r.Exports)
	}
}

func TestCancelledStorageDiagnosticsDoNotCreateState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(t.TempDir(), "missing")
	r := inspectStorage(ctx, dir)
	if r.Database.Status != "unavailable" || r.Exports.Status != "unavailable" {
		t.Fatal("cancelled scan claimed an observation", r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("cancelled scan created state", err)
	}
}

func TestGuidancePreservesUnverifiedLiveAcceptance(t *testing.T) {
	r := DoctorReport{Verification: map[string]string{"local": "passed", "feishuIdentity": "passed"}}
	steps := diagnosticNextSteps(r, true, true)
	if len(steps) < 2 || r.Verification["messageDelivery"] == "passed" {
		t.Fatal("identity or local checks promoted to live acceptance")
	}
	for _, name := range []string{"Claude Code", "Feishu identity probe", "model endpoint basic probe", "data directory / SQLite"} {
		if diagnosticRemedy(name) == "" {
			t.Fatal("missing recovery guidance", name)
		}
	}
}

func TestExportMetadataScanStopsAtItsBudget(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := scanExportMetadata(context.Background(), dir, 2)
	if r.Status != "partial" || r.Files != 2 || r.Bytes != 2 {
		t.Fatal("export scan exceeded its entry budget or hid truncation", r)
	}
}
