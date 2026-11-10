package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/feedbackexport"
	"github.com/sean2077/feishu-bot/internal/store"
)

const feedbackHelp = `Export reply-card feedback as JSON Lines.

Usage:
  feishu-bot feedback export [--config PATH] [--value useless|useful|all]
                             [--since RFC3339|DAYS] [--project ID]

--value defaults to useless. --since accepts an RFC3339 timestamp or a
non-negative number of days before now. Output contains user questions,
answer summaries and open_ids; treat the exported file as sensitive.
`

func parseFeedback(args []string, o Options) (Options, error) {
	o.FeedbackValue = "useless"
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			o.Command = "help"
			o.Subcommand = "feedback"
			return o, nil
		}
		matched := false
		for name, dst := range map[string]*string{"--config": &o.ConfigPath, "--value": &o.FeedbackValue, "--since": &o.FeedbackSince, "--project": &o.FeedbackProject} {
			if arg != name && !strings.HasPrefix(arg, name+"=") {
				continue
			}
			if err := mark(seen, name); err != nil {
				return o, err
			}
			value, _, err := takeValue(args, &i, arg, name)
			if err != nil {
				return o, err
			}
			*dst = value
			if name == "--config" {
				o.ConfigExplicit = true
			}
			matched = true
			break
		}
		if !matched {
			return o, errors.New("unexpected option; see feishu-bot feedback --help")
		}
	}
	if o.FeedbackValue != "useless" && o.FeedbackValue != "useful" && o.FeedbackValue != "all" {
		return o, errors.New("--value must be useless, useful or all")
	}
	return resolveOptions(&o)
}

func runFeedbackCommand(ctx context.Context, o Options, out io.Writer) error {
	if o.Subcommand != "export" {
		return errors.New("unknown feedback command")
	}
	path, found, err := resolveConfigPath(o)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("configuration is missing")
	}
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	since, err := feedbackSince(o.FeedbackSince)
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		return err
	}
	defer st.Close()
	var present int
	if err := st.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='feedback'").Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	records, err := feedbackexport.Query(ctx, st.DB, feedbackexport.Filter{Since: since, Value: exportValue(o.FeedbackValue), Project: o.FeedbackProject})
	if err != nil {
		return err
	}
	return feedbackexport.Encode(out, records)
}

func exportValue(value string) string {
	if value == "all" {
		return ""
	}
	return value
}

func feedbackSince(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if days, err := strconv.Atoi(value); err == nil {
		if days < 0 {
			return 0, errors.New("--since days must be non-negative")
		}
		return time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli(), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, errors.New("--since must be RFC3339 or a number of days")
	}
	return parsed.UnixMilli(), nil
}
