package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/feishu"
	"github.com/sean2077/feishu-bot/internal/gateway"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

const help = `feishu-bot init [--config path]
feishu-bot doctor [--config path] [--env-file path] [--probe] [--probe-feishu]
                  [--verify-chat configured-chat-id|dm:configured-user-id]
feishu-bot run [--config path] [--env-file path] [--log-file path]
               [--log-max-size MB] [--log-max-backups N] [--service]
feishu-bot daemon install|uninstall|start|stop|restart|status|logs
feishu-bot config path|example|format|explain
feishu-bot session run --session FULL_SESSION_ID [--config path] [--env-file path]
feishu-bot attach --config path [--user OPEN_ID] [--title TEXT]
                  [--summary-file path|-] [--native ID] [--holder-pid PID] [--json]
feishu-bot notify --config path --summary-file path|- [--title TEXT]
                  [--native ID] [--holder-pid PID] [--user OPEN_ID] [--json]
feishu-bot topic reply --config path --native ID --message-id om_xxx --text-file - [--json]
feishu-bot action <operation> '<parameters-json>'
feishu-bot context
feishu-bot feedback export [--config path] [--value useless|useful|all]
                           [--since RFC3339|days] [--project id]
feishu-bot version

Feedback export contains user questions, answer summaries and open_ids;
treat the output as sensitive.

Doctor is offline by default. --probe-feishu reads application identity;
--probe sends a potentially billable model request. Neither verifies live collaboration.
--verify-chat explicitly sends a test card/file and waits for authorized confirmation;
stop the local service first. It never starts a model or changes chat authorization.
On Linux and Windows, daemon install copies this binary into a work directory,
writes service.env, registers a logon service and starts it with no launcher scripts.
On macOS, use run in the foreground or with your own service manager.
Linux / macOS / Windows: one Go service starts native Claude Code sessions on demand
and reuses their processes between messages. Node.js is not a runtime dependency.
`

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitCodeError) Unwrap() error { return e.err }

// Run executes a CLI command. It never exits the process, allowing embedding and
// cancellation before startup. All credentials are redacted from diagnostics.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var secrets []string
	errOut := io.Writer(stderr)
	err := func() error {
		o, err := Parse(args)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil && o.Command != "help" && o.Command != "version" {
			if o.Command == "run" {
				return nil
			}
			return err
		}
		switch o.Command {
		case "help":
			text := help
			if o.Subcommand == "daemon" {
				text = daemonHelp
			}
			if o.Subcommand == "config" {
				text = configHelp
			}
			if o.Subcommand == "session" {
				text = sessionHelp
			}
			if o.Subcommand == "attach" {
				text = attachHelp
			}
			if o.Subcommand == "notify" {
				text = notifyHelp
			}
			if o.Subcommand == "topic" {
				text = topicHelp
			}
			if o.Subcommand == "feedback" {
				text = feedbackHelp
			}
			_, err = io.WriteString(stdout, text)
			return err
		case "version":
			_, err = fmt.Fprintf(stdout, "feishu-bot %s\n", buildVersion())
			return err
		case "init":
			return initialize(o.ConfigPath, stdout)
		case "action":
			secrets = []string{os.Getenv("FEISHU_ACTION_TOKEN")}
			return native.RunActionClient(ctx, o.ActionArgs, stdout)
		case "context":
			secrets = []string{os.Getenv("FEISHU_ACTION_TOKEN")}
			return native.RunContextClient(ctx, stdout)
		case "feedback":
			return runFeedbackCommand(ctx, o, stdout)
		case "daemon":
			return runDaemon(ctx, o, stdout, stderr)
		case "config":
			return runConfigCommand(ctx, o, stdout, stderr)
		}
		if o.EnvFile != "" {
			if err := LoadEnvFile(o.EnvFile); err != nil {
				return err
			}
		}
		var logWriter *RotatingWriter
		if o.Command == "run" && o.LogFile != "" {
			logWriter, err = openLogFile(o.LogFile, o.LogSize, o.LogKeep)
			if err != nil {
				return err
			}
			defer logWriter.Close()
			errOut = io.MultiWriter(logWriter, stderr)
			if o.Service {
				detachConsole()
			}
		}
		c, err := config.Load(o.ConfigPath)
		if err != nil {
			if o.Command == "notify" || o.Command == "topic" {
				return &exitCodeError{code: 3, err: err}
			}
			if os.IsNotExist(err) {
				return errors.New("configuration is missing; run feishu-bot init --config <path> first")
			}
			return err
		}
		secrets = config.Secrets(c)
		if o.Command == "session" {
			return runLocalSession(ctx, c, o, stdout, stderr)
		}
		if o.Command == "attach" {
			return runAttach(ctx, c, o, stdout)
		}
		if o.Command == "notify" {
			return runNotify(ctx, c, o, stdout)
		}
		if o.Command == "topic" {
			return runTopicReply(ctx, c, o, stdout)
		}
		if o.Command == "doctor" {
			report := RunDoctor(ctx, c, o.Probe, o.ProbeFeishu)
			if o.VerifyChat != "" {
				live := VerifyConfiguredChat(ctx, c, o.VerifyChat, stderr)
				report.LiveChat = &live
				report.Scope = "Local checks, explicitly requested remote probes and an explicit test-chat exchange; native/model tools remain independently unverified."
				for key, state := range live.Checks {
					report.Verification[key] = state
				}
				report.Ready = report.Ready && live.Status == "passed"
			}
			data, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintln(stdout, core.Redact(string(data), secrets)); err != nil {
				return err
			}
			if !report.Ready {
				return errors.New("doctor found failed or blocked checks")
			}
			return nil
		}
		var failed []string
		for _, check := range deploymentChecks(c) {
			if !check.OK {
				failed = append(failed, check.Name+": "+check.Detail)
			}
		}
		if len(failed) > 0 {
			return errors.New("startup configuration is incomplete; run feishu-bot doctor\n" + strings.Join(failed, "\n"))
		}
		logOut := io.Writer(stderr)
		if logWriter != nil {
			logOut = logWriter
		}
		log := jsonLogger(logOut, secrets)
		return runService(ctx, c, log, newService)
	}()
	if err != nil {
		var coded *exitCodeError
		if errors.As(err, &coded) {
			if coded.err != nil {
				_, _ = fmt.Fprintln(errOut, core.Redact(coded.err.Error(), secrets))
			}
			return coded.code
		}
		_, _ = fmt.Fprintln(errOut, core.Redact(err.Error(), secrets))
		return 1
	}
	return 0
}

func openLogFile(path string, maxSize, backups int) (*RotatingWriter, error) {
	if maxSize <= 0 {
		maxSize = 10
	}
	return NewRotatingWriter(path, int64(maxSize)*1024*1024, backups)
}

func initialize(path string, out io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return errors.New("configuration already exists; it was not overwritten")
		}
		return err
	}
	_, writeErr := io.WriteString(f, config.Example)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	_, err = fmt.Fprintf(out, "Created %s.\nFill in application, chat, member and model settings; set credentials in the service environment.\nRun feishu-bot doctor --config <path> for local checks.\nExplicitly opt in to --probe-feishu for bot identity and --probe for a potentially billable model request.\nVerify messages, card callbacks and file delivery in an authorized test chat.\nSee docs/deployment.md and docs/validation.md.\n", path)
	return err
}

func jsonLogger(out io.Writer, secrets []string) core.Logger {
	var mu sync.Mutex
	return func(event string, detail core.Args) {
		entry := core.Args{"at": time.Now().UTC().Format(time.RFC3339Nano), "event": event}
		for k, v := range detail {
			if k != "event" && k != "at" {
				entry[k] = v
			}
		}
		b, err := json.Marshal(entry)
		if err != nil {
			b = []byte(`{"event":"log_encoding_failed"}`)
		}
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintln(out, core.Redact(string(b), secrets))
	}
}

type serviceGateway interface {
	Recover(context.Context) error
	Message(context.Context, core.Message) error
	ActionResponse(context.Context, core.CardAction) (core.Args, error)
	Close(context.Context) error
}
type serviceChannel interface {
	Start(context.Context, func(context.Context, core.Message) error, func(context.Context, core.CardAction) (core.Args, error)) error
	Close() error
}
type serviceFactory func(core.Config, *store.Store, core.Logger) (serviceGateway, serviceChannel, error)

func newService(c core.Config, st *store.Store, log core.Logger) (serviceGateway, serviceChannel, error) {
	adapter, err := feishu.New(c, st.DB, log)
	if err != nil {
		return nil, nil, err
	}
	g := gateway.New(c, st, adapter, native.NewFactory(c, log), log)
	return g, adapter, nil
}

// runService keeps recovery, event admission, shutdown and database lifetime in
// one scope. An unconfirmed process stop keeps the exclusive data-directory lock.
func runService(parent context.Context, c core.Config, log core.Logger, makeService serviceFactory) (result error) {
	if parent.Err() != nil {
		return nil
	}
	unlock, err := workspace.Lock(c.DataDir)
	if err != nil {
		return err
	}
	var st *store.Store
	var g serviceGateway
	var channel serviceChannel
	var stopRequested atomic.Bool
	ctx, cancel := context.WithCancel(parent)
	defer func() {
		cancel()
		// Start has returned (or admission never began), and cancellation stops
		// the inbound stream. Keep REST transport alive until approval cards and
		// other final gateway updates have been settled.
		confirmed := true
		if g != nil {
			log("shutdown", nil)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := g.Close(stopCtx)
			stopCancel()
			if err != nil {
				confirmed = false
				result = errors.Join(result, err)
				log("shutdown_unconfirmed", core.Args{"error": err.Error()})
			}
		}
		if channel != nil {
			if err := channel.Close(); err != nil {
				result = errors.Join(result, err)
			}
		}
		if st != nil {
			result = errors.Join(result, st.Close())
		}
		if confirmed {
			result = errors.Join(result, unlock())
		}
	}()
	stopPath := stopRequestPath(c.DataDir)
	if err := clearStopRequest(stopPath); err != nil {
		return err
	}
	st, err = store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		return err
	}
	g, channel, err = makeService(c, st, log)
	if err != nil {
		return err
	}
	if g == nil || channel == nil {
		return errors.New("service initialization returned an incomplete gateway or transport")
	}
	go watchStop(ctx, stopPath, cancel, func() { stopRequested.Store(true) }, log)
	if err = g.Recover(ctx); err != nil {
		if parent.Err() != nil || stopRequested.Load() {
			if stopRequested.Load() && log != nil {
				log("recover_stopped", core.Args{"error": err.Error()})
			}
			return nil
		}
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	if attaches, ok := g.(attachGateway); ok {
		go watchAttach(ctx, attaches, c.DataDir, log)
	}
	log("starting", core.Args{"runtime": "go", "groups": len(c.Groups), "dms": len(c.DMs), "maxWorkers": c.Runtime.MaxWorkers})
	err = channel.Start(ctx, g.Message, g.ActionResponse)
	if parent.Err() != nil {
		return nil
	}
	if stopRequested.Load() {
		if err != nil && log != nil {
			log("channel_stopped", core.Args{"error": err.Error()})
		}
		return nil
	}
	return err
}

const stopRequestFile = "gateway.stop"
const stopPollInterval = 250 * time.Millisecond

func stopRequestPath(dataDir string) string {
	return filepath.Join(dataDir, stopRequestFile)
}

func clearStopRequest(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect stop control file")
	}
	if !info.Mode().IsRegular() {
		return errors.New("stop control path is not a regular file")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove stale stop control file")
	}
	return nil
}

func watchStop(ctx context.Context, path string, cancel context.CancelFunc, mark func(), log core.Logger) {
	ticker := time.NewTicker(stopPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Lstat(path)
			if err == nil && info.Mode().IsRegular() {
				if mark != nil {
					mark()
				}
				if log != nil {
					log("stop_requested", core.Args{"source": "control_file"})
				}
				cancel()
				return
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) && log != nil {
				log("stop_request_check_failed", core.Args{"error": "cannot inspect control file"})
			}
		}
	}
}
