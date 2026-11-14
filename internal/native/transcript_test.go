package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func transcriptFixture(t *testing.T, cwd string) (string, string) {
	t.Helper()
	config := t.TempDir()
	native := "11111111-2222-3333-4444-555555555555"
	dir := filepath.Join(config, "projects", EncodeProjectDir(cwd))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, native+".jsonl")
	lines := []string{
		`{"type":"mode","mode":"normal","sessionId":"` + native + `"}`,
		`{"type":"user","cwd":` + quote(cwd) + `,"message":{"role":"user"}}`,
		`{"type":"assistant","cwd":` + quote(cwd) + `,"message":{"role":"assistant","model":"model-a"}}`,
		`{"type":"assistant","cwd":` + quote(cwd) + `,"message":{"role":"assistant","model":"model-b"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return config, native
}

func quote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

func TestLocateTranscript(t *testing.T) {
	cwd := t.TempDir()
	config, native := transcriptFixture(t, cwd)
	tr, err := LocateTranscript(config, native)
	if err != nil {
		t.Fatal(err)
	}
	if tr.CWD != cwd || tr.Model != "model-b" || !strings.HasSuffix(tr.Path, native+".jsonl") {
		t.Fatalf("transcript: %+v", tr)
	}
}

func TestLocateTranscriptRejectsAmbiguityAndMismatch(t *testing.T) {
	cwd := t.TempDir()
	config, native := transcriptFixture(t, cwd)
	other := filepath.Join(config, "projects", "wrong-dir")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, native+".jsonl"), []byte(`{"cwd":`+quote(cwd)+`}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LocateTranscript(config, native); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("ambiguous transcript accepted: %v", err)
	}
	if _, err := LocateTranscript(config, "../escape"); err == nil {
		t.Fatal("path traversal native ID accepted")
	}
	if _, err := LocateTranscript(config, "missing-native"); err == nil {
		t.Fatal("missing transcript accepted")
	}
}

func TestLocateTranscriptVerifiesEncodedDirectory(t *testing.T) {
	cwd := t.TempDir()
	config := t.TempDir()
	native := "session-mismatch"
	dir := filepath.Join(config, "projects", "not-the-cwd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, native+".jsonl"), []byte(`{"cwd":`+quote(cwd)+`}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LocateTranscript(config, native); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched project directory accepted: %v", err)
	}
}

func TestEncodeProjectDir(t *testing.T) {
	cases := map[string]string{
		`C:\Users\admin\Workspace\project`: "C--Users-admin-Workspace-project",
		`/home/user/my_project.v2`:         "-home-user-my-project-v2",
		"plain":                            "plain",
	}
	for input, want := range cases {
		if got := EncodeProjectDir(input); got != want {
			t.Fatalf("EncodeProjectDir(%q) = %q, want %q", input, got, want)
		}
	}
}
