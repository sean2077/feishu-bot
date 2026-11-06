package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/gateway"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

const sessionHelp = `Resume an existing gateway session in the local native terminal.

Usage:
  feishu-bot session run --session FULL_GATEWAY_SESSION_ID [--config PATH] [--env-file PATH]

Stop the gateway first (daemon stop, or stop its foreground run). Copy the full
gateway session ID from /sessions; native IDs and ambiguous names are not accepted.
The local command holds gateway.lock and owns the native process tree until exit.
The saved cwd, configuration identity and applied model profile must still match.
Queued, uncertain or unfinished inputs/actions/approvals block handoff.
Native TUI permission prompts are answered locally. Feishu tools and project action
relays are unavailable, and local output is not forwarded to Feishu. Local usage is
marked outside gateway coverage. After exit, restart the gateway to continue there.
`

func parseSession(args []string, o Options) (Options, error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			o.Command = "help"
			o.Subcommand = "session"
			return o, nil
		}
		matched := false
		for name, dst := range map[string]*string{"--session": &o.SessionID, "--config": &o.ConfigPath, "--env-file": &o.EnvFile} {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				if err := mark(seen, name); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, name)
				if err != nil {
					return o, err
				}
				*dst = value
				matched = true
				if name == "--config" {
					o.ConfigExplicit = true
				}
				break
			}
		}
		if !matched {
			return o, errors.New("unexpected option; see feishu-bot session --help")
		}
	}
	if o.SessionID == "" || len(o.SessionID) > 64 {
		return o, errors.New("session run requires --session FULL_GATEWAY_SESSION_ID")
	}
	return resolveOptions(&o)
}

type localSessionRunner func(context.Context, core.Config, core.Room, core.Session, core.Workspace) error

func runLocalSession(ctx context.Context, c core.Config, o Options, out, errOut io.Writer) error {
	return runSessionWith(ctx, c, o, out, func(ctx context.Context, c core.Config, r core.Room, s core.Session, w core.Workspace) error {
		return native.Interactive(ctx, c, r, s, w, os.Stdin, out, errOut)
	})
}

// The existing data-directory lock is the handoff lease. There is never a live
// gateway plus a terminal writer, and unconfirmed descendants retain the lock.
func runSessionWith(ctx context.Context, c core.Config, o Options, out io.Writer, run localSessionRunner) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	dbPath := filepath.Join(c.DataDir, "gateway.sqlite")
	if info, err := os.Stat(dbPath); err != nil || !info.Mode().IsRegular() {
		return errors.New("existing gateway state is required for local handoff")
	}
	unlock, err := workspace.Lock(c.DataDir)
	if err != nil {
		return fmt.Errorf("stop the gateway before local handoff: %w", err)
	}
	confirmed := true
	defer func() {
		// Workspace preparation can own a process tree before the native TUI
		// starts. An unconfirmed setup exit retains the same handoff lease.
		if confirmed && !errors.Is(result, process.ErrUnconfirmed) {
			result = errors.Join(result, unlock())
		}
	}()
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, st.Close()) }()
	var s core.Session
	var cc sql.NullString
	if err = st.DB.QueryRowContext(ctx, "SELECT id,room,cc,state,created FROM sessions WHERE id=?", o.SessionID).Scan(&s.ID, &s.RoomKey, &cc, &s.State, &s.CreatedAt); err != nil {
		return errors.New("full gateway session ID was not found")
	}
	s.CCID = cc.String
	if s.CCID == "" {
		return errors.New("session has no native identity; first establish it through Feishu")
	}
	r, ok := gateway.RoomFromKey(c, s.RoomKey)
	if !ok {
		return errors.New("session room is no longer authorized by this configuration")
	}
	var outstanding int
	if err = st.DB.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM inbox WHERE room=? AND status IN ('queued','sent','consumed','uncertain'))+
 (SELECT count(*) FROM requests WHERE room=? AND state='pending')+
 (SELECT count(*) FROM action_runs WHERE room=? AND state NOT IN ('done','failed'))+
 (SELECT count(*) FROM outbox WHERE room=? AND state NOT IN ('done','cancelled'))`, r.Key, r.Key, r.Key, r.Key).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding > 0 {
		return errors.New("session room has pending or uncertain inputs, outputs, approvals or actions; resolve them before local handoff")
	}
	prefs, err := st.Preferences(r.Key, s.ID)
	if err != nil {
		return err
	}
	if prefs.RequestedProfile != nil {
		return errors.New("apply or clear the pending model selection in Feishu before local handoff")
	}
	s.Profile = prefs.Profile
	p, ok := c.Project(r.ProjectID)
	if !ok {
		return errors.New("configured project was not found")
	}
	w, err := workspace.PrepareContext(ctx, c, r, s, p)
	if err != nil {
		return err
	}
	if err = st.BindLaunchIdentity(s, w, p.CWD != ""); err != nil {
		return err
	}
	if _, err = st.Resume(r.Key, s.ID); err != nil {
		return err
	}
	s.State = "open"
	// Native local turns are not observed by the gateway's SDK result ledger.
	if _, err = st.DB.ExecContext(ctx, "UPDATE usage_coverage SET prior_unknown=1 WHERE session=?", s.ID); err != nil {
		return err
	}
	if _, err = st.DB.ExecContext(ctx, "INSERT INTO audit(at,room,event,detail) VALUES(?,?,'local_session_started',?)", time.Now().UnixMilli(), r.Key, sessionDetail(core.Args{"session": s.ID, "nativeSessionId": s.CCID})); err != nil {
		return err
	}
	if _, err = fmt.Fprintf(out, "Local session %s; gateway remains locked until native exit is confirmed.\n", s.ID); err != nil {
		return err
	}
	result = run(ctx, c, r, s, w)
	confirmed = !errors.Is(result, process.ErrUnconfirmed)
	state := "exited"
	if result != nil {
		state = "failed"
	}
	if !confirmed {
		state = "termination_unconfirmed"
	}
	_, auditErr := st.DB.Exec("INSERT INTO audit(at,room,event,detail) VALUES(?,?,'local_session_finished',?)", time.Now().UnixMilli(), r.Key, sessionDetail(core.Args{"session": s.ID, "state": state}))
	result = errors.Join(result, auditErr)
	if confirmed {
		_, _ = fmt.Fprintln(out, "Local session ended. Restart the gateway to continue the selected session in Feishu.")
	}
	return result
}

func sessionDetail(v core.Args) string { data, _ := json.Marshal(v); return string(data) }
