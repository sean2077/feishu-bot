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
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/feishu"
	"github.com/sean2077/feishu-bot/internal/gateway"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

// Version can be set with -ldflags '-X github.com/sean2077/feishu-bot/internal/cli.Version=...'.
var Version = "0.3.0-dev"

const help = `feishu-bot init [--config path]
feishu-bot doctor [--config path] [--probe] [--probe-feishu]
feishu-bot run [--config path]
feishu-bot version

Doctor is offline by default. --probe-feishu reads application identity;
--probe sends a potentially billable model request. Neither verifies live collaboration.
Linux / Windows: one Go service starts native Claude Code sessions on demand
and reuses their processes between messages. Node.js is not a runtime dependency.
`

// Run executes a CLI command. It never exits the process, allowing embedding and
// cancellation before startup. All credentials are redacted from diagnostics.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var secrets []string
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
			_, err = io.WriteString(stdout, help)
			return err
		case "version":
			_, err = fmt.Fprintf(stdout, "feishu-bot %s\n", Version)
			return err
		case "init":
			return initialize(o.ConfigPath, stdout)
		case "action":
			secrets = []string{os.Getenv("FEISHU_ACTION_TOKEN")}
			return native.RunActionClient(ctx, o.ActionArgs, stdout)
		}
		c, err := config.Load(o.ConfigPath)
		if err != nil {
			if os.IsNotExist(err) {
				return errors.New("configuration is missing; run feishu-bot init --config <path> first")
			}
			return err
		}
		secrets = config.Secrets(c)
		if o.Command == "doctor" {
			report := RunDoctor(ctx, c, o.Probe, o.ProbeFeishu)
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
		log := jsonLogger(stderr, secrets)
		return runService(ctx, c, log, newService)
	}()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, core.Redact(err.Error(), secrets))
		return 1
	}
	return 0
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
	Action(context.Context, core.CardAction) (string, error)
	Close(context.Context) error
}
type serviceChannel interface {
	Start(context.Context, func(context.Context, core.Message) error, func(context.Context, core.CardAction) (string, error)) error
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
	if err = g.Recover(ctx); err != nil {
		if parent.Err() != nil {
			return nil
		}
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	log("starting", core.Args{"runtime": "go", "groups": len(c.Groups), "dms": len(c.DMs), "maxWorkers": c.Runtime.MaxWorkers})
	err = channel.Start(ctx, g.Message, g.Action)
	if parent.Err() != nil {
		return nil
	}
	return err
}
