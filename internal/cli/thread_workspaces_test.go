package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestDoctorReportsMissingTopicWorkspaceWithoutCreatingIt(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.Runtime.ClaudeExecutable = filepath.Join(c.DataDir, "missing-claude")
	c.Groups = []core.Group{{ChatID: "oc_topics", ThreadWorkspaces: map[string]string{"om_present": t.TempDir(), "om_missing": filepath.Join(c.DataDir, "not-created")}}}
	report := RunDoctor(context.Background(), c, false, false)
	checks := map[string]bool{}
	for _, check := range report.Checks {
		if strings.Contains(check.Name, "thread workspace") {
			checks[check.Name] = check.OK
		}
	}
	if len(checks) != 2 || !checks["oc_topics thread workspace om_present"] || checks["oc_topics thread workspace om_missing"] {
		t.Fatalf("topic directory diagnostics: %+v", checks)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "not-created")); !os.IsNotExist(err) {
		t.Fatal("doctor created a configured topic directory", err)
	}
}

func TestLocalTopicHandoffRefusesBindingLegacySessionToNewDirectory(t *testing.T) {
	c, err := config.Parse([]byte(config.Example), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Groups[0].SessionScope = core.SessionScopeThread
	c.Groups[0].ThreadWorkspaces = map[string]string{"om_root": t.TempDir()}
	r, ok := c.ThreadRoomFor(c.Groups[0].ChatID, "om_root")
	if !ok {
		t.Fatal("missing topic")
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Session(r.Key)
	if err == nil {
		err = st.SetCC(s.ID, core.NewID())
	}
	if closeErr := st.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &bytes.Buffer{}, func(context.Context, core.Config, core.Room, core.Session, core.Workspace, func() error) error {
		called = true
		return nil
	})
	if err == nil || called || !strings.Contains(err.Error(), "旧会话缺少接入身份记录") {
		t.Fatal("local handoff silently rebound a legacy native session", err)
	}
}
