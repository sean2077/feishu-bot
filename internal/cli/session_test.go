package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localSessionFixture(t *testing.T) (core.Config, core.Room, core.Session, core.Workspace) {
	t.Helper()
	c, err := config.Parse([]byte(config.Example), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, ok := c.RoomFor("oc_example_group", "")
	if !ok {
		t.Fatal("missing room")
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Project(r.ProjectID)
	w, err := workspace.Prepare(c, r, s, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BindLaunchIdentity(s, w, false); err != nil {
		t.Fatal(err)
	}
	s.CCID = core.NewID()
	if err := st.SetCC(s.ID, s.CCID); err != nil {
		t.Fatal(err)
	}
	return c, r, s, w
}

func TestLocalSessionRetainsIdentityAndExclusiveLease(t *testing.T) {
	c, r, s, w := localSessionFixture(t)
	var out bytes.Buffer
	err := runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &out, func(ctx context.Context, actual core.Config, room core.Room, session core.Session, work core.Workspace, commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		if session.ID != s.ID || session.CCID != s.CCID || room.Key != r.Key || work.Work != w.Work || work.ConfigDir != w.ConfigDir || work.NativeConfig != w.NativeConfig {
			t.Fatal("handoff changed launch identity", session, work)
		}
		if unlock, err := workspace.Lock(c.DataDir); err == nil {
			_ = unlock()
			t.Fatal("gateway could acquire handoff lease")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "gateway.lock")); !os.IsNotExist(err) {
		t.Fatal("confirmed handoff retained lock", err)
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	selected, err := st.Session(r.Key)
	if err != nil || selected.ID != s.ID || selected.CCID != s.CCID {
		t.Fatal("handoff changed durable mapping", selected, err)
	}
	var unknown int
	if err := st.DB.QueryRow("SELECT prior_unknown FROM usage_coverage WHERE session=?", s.ID).Scan(&unknown); err != nil || unknown != 1 {
		t.Fatal("local usage falsely included in coverage", unknown, err)
	}
}

func TestLocalSessionFailsClosedOnActiveLockUnknownInputAndUnconfirmedExit(t *testing.T) {
	c, r, s, _ := localSessionFixture(t)
	var out bytes.Buffer
	calls := 0
	run := func(_ context.Context, _ core.Config, _ core.Room, _ core.Session, _ core.Workspace, commit func() error) error {
		calls++
		if err := commit(); err != nil {
			return err
		}
		return process.ErrUnconfirmed
	}
	unlock, err := workspace.Lock(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &out, run); err == nil || calls != 0 {
		t.Fatal("handoff launched while gateway owned lock", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec("INSERT INTO inbox(room,id,message,status,at,session) VALUES(?,'unknown','{}','sent',1,?)", r.Key, s.ID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &out, run); err == nil || calls != 0 {
		t.Fatal("handoff resumed while an input was still in flight", err)
	}
	st, err = store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// Recovery turns an interrupted input into uncertain evidence. It is
	// reported and never replayed, but it does not block handoff forever.
	_, err = st.DB.Exec("UPDATE inbox SET status='uncertain' WHERE id='unknown'")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &out, run); !errors.Is(err, process.ErrUnconfirmed) || calls != 1 {
		t.Fatal("unconfirmed exit hidden", calls, err)
	}
	if !strings.Contains(out.String(), "remain uncertain") {
		t.Fatal("uncertain evidence was not reported", out.String())
	}
	if _, err := workspace.ReadLock(c.DataDir); err != nil {
		t.Fatal("unconfirmed exit released lease", err)
	}
}

func TestLocalSessionRejectsChangedConfigurationAndInvalidArguments(t *testing.T) {
	c, _, s, _ := localSessionFixture(t)
	c.Projects[0].CWD = t.TempDir()
	called := false
	err := runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &bytes.Buffer{}, func(context.Context, core.Config, core.Room, core.Session, core.Workspace, func() error) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatal("changed cwd silently accepted", err)
	}
	for _, args := range [][]string{{"session", "run"}, {"session", "run", "--session", "id", "--session", "id"}, {"session", "resume", "id"}} {
		if _, err := Parse(args); err == nil {
			t.Fatal("invalid session args accepted", args)
		}
	}
}

func TestLocalSessionRejectsUndeliveredOutput(t *testing.T) {
	c, r, s, _ := localSessionFixture(t)
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB.Exec("INSERT INTO outbox(id,room,session,text,success,state,at) VALUES('unsent',?,?,'answer',1,'pending',1)", r.Key, s.ID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	called := false
	err = runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &bytes.Buffer{}, func(context.Context, core.Config, core.Room, core.Session, core.Workspace, func() error) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatal("handoff ignored pending output", err)
	}
}

func TestLocalSessionFailedLaunchKeepsChatSelection(t *testing.T) {
	c, r, s, _ := localSessionFixture(t)
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := st.NewSession(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	launchErr := errors.New("missing model credential or endpoint")
	err = runSessionWith(context.Background(), c, Options{SessionID: s.ID}, &bytes.Buffer{}, func(context.Context, core.Config, core.Room, core.Session, core.Workspace, func() error) error {
		return launchErr
	})
	if !errors.Is(err, launchErr) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "gateway.lock")); !os.IsNotExist(err) {
		t.Fatal("failed launch retained lock", err)
	}
	st, err = store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	selected, err := st.Session(r.Key)
	if err != nil || selected.ID != current.ID {
		t.Fatal("failed launch switched the chat session", selected.ID, current.ID, err)
	}
	var unknown int
	if err := st.DB.QueryRow("SELECT coalesce(max(prior_unknown),0) FROM usage_coverage WHERE session=?", s.ID).Scan(&unknown); err != nil || unknown != 0 {
		t.Fatal("failed launch marked local usage", unknown, err)
	}
	var started int
	if err := st.DB.QueryRow("SELECT count(*) FROM audit WHERE event IN ('local_session_started','local_session_finished')").Scan(&started); err != nil || started != 0 {
		t.Fatal("failed launch audited a local session", started, err)
	}
}
