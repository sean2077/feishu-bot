package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
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

type feedbackExport struct {
	At              string `json:"at"`
	Project         string `json:"project"`
	Room            string `json:"room"`
	Session         string `json:"session"`
	NativeSessionID string `json:"nativeSessionId"`
	Value           string `json:"value"`
	User            string `json:"user"`
	Question        string `json:"question"`
	QuestionMissing bool   `json:"questionMissing,omitempty"`
	AnswerSummary   string `json:"answerSummary"`
	MessageID       string `json:"messageId"`
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
	q := "SELECT at,project,room,session,native_session,value,user,question,answer_summary,message FROM feedback WHERE at>=?"
	args := []any{since}
	if o.FeedbackValue != "all" {
		q += " AND value=?"
		args = append(args, o.FeedbackValue)
	}
	if o.FeedbackProject != "" {
		q += " AND project=?"
		args = append(args, o.FeedbackProject)
	}
	q += " ORDER BY at,id"
	rows, err := st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(out)
	for rows.Next() {
		var at int64
		var project, room, session, value, user, question, answer, message string
		var native sql.NullString
		if err := rows.Scan(&at, &project, &room, &session, &native, &value, &user, &question, &answer, &message); err != nil {
			return err
		}
		record := feedbackExport{At: time.UnixMilli(at).UTC().Format(time.RFC3339Nano), Project: project, Room: room, Session: session, NativeSessionID: native.String, Value: value, User: user, Question: question, QuestionMissing: question == "", AnswerSummary: answer, MessageID: message}
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return rows.Err()
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
