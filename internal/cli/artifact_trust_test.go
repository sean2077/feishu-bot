package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/process"
)

func artifactTrustEnvironment(t *testing.T, certificate []byte, directory string) []string {
	t.Helper()
	path := filepath.Join(directory, "fixture-ca.pem")
	if err := os.WriteFile(path, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		return []string{"SSL_CERT_FILE=" + path}
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Fatal("unsupported artifact fixture trust platform")
	}
	// Trust changes belong to explicit hosted-CI test setup, never production
	// code or a developer's personal/self-hosted machine. The Python helper
	// journals exact fingerprints and also supplies an always-run CI cleanup.
	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "artifact_trust.py"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.Command("python", append([]string{script}, args...)...)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		err := process.Run(ctx, cmd)
		return strings.TrimSpace(output.String()), err
	}
	record, err := run("install", "--certificate", path)
	if err != nil {
		t.Fatal("cannot prepare isolated runner TLS trust", err, record)
	}
	if record == "" || strings.ContainsAny(record, "\r\n") {
		t.Fatal("invalid fixture trust cleanup record")
	}
	t.Cleanup(func() {
		if output, err := run("cleanup", "--record", record); err != nil {
			t.Error("temporary runner certificate cleanup failed", err, output)
		}
	})
	return nil
}
