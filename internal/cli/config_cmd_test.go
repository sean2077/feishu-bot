package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathExampleAndFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := Options{Command: "config", Subcommand: "path", ConfigPath: path, ConfigExplicit: true}
	var stdout, stderr bytes.Buffer
	if err := runConfigCommand(context.Background(), o, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != path || stderr.Len() != 0 {
		t.Fatalf("path output = %q, stderr = %q", stdout.String(), stderr.String())
	}

	o.Subcommand = "format"
	stdout.Reset()
	if err := runConfigCommand(context.Background(), o, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || !strings.Contains(string(data), "\n  \"a\": 1") {
		t.Fatalf("formatted configuration = %q", data)
	}

	if err := os.WriteFile(path, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runConfigCommand(context.Background(), o, &stdout, &stderr); err == nil {
		t.Fatal("formatted invalid JSON")
	}

	o.Subcommand = "example"
	stdout.Reset()
	if err := runConfigCommand(context.Background(), o, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatal("config example is not valid JSON")
	}
}

func TestConfigPathNotFoundStillPrints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	o := Options{Command: "config", Subcommand: "path", ConfigPath: path, ConfigExplicit: true}
	var stdout, stderr bytes.Buffer
	if err := runConfigCommand(context.Background(), o, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != path || !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
