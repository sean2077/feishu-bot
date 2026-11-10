package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestFeedbackExportDoctorChecks(t *testing.T) {
	c := core.Config{Feedback: core.FeedbackSettings{Export: core.FeedbackExportConfig{Enabled: true, Dir: t.TempDir()}}}
	checks := feedbackExportChecks(c)
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("writable directory: %+v", checks)
	}
	c.Feedback.Export.Enabled = false
	if checks := feedbackExportChecks(c); len(checks) != 0 {
		t.Fatalf("disabled export was checked: %+v", checks)
	}

	c.Feedback.Export = core.FeedbackExportConfig{Enabled: true, Dir: filepath.Join(t.TempDir(), "missing")}
	if checks := feedbackExportChecks(c); len(checks) != 1 || checks[0].OK {
		t.Fatalf("missing directory accepted: %+v", checks)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	c.Feedback.Export.Dir = file
	if checks := feedbackExportChecks(c); len(checks) != 1 || checks[0].OK {
		t.Fatalf("regular file accepted: %+v", checks)
	}

	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	c.Feedback.Export.Dir = link
	if checks := feedbackExportChecks(c); len(checks) != 1 || checks[0].OK {
		t.Fatalf("symlink accepted: %+v", checks)
	}
}
