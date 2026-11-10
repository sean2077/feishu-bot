// Package feedbackexport reads reply-card feedback into JSON Lines records.
// The CLI and the scheduled maintenance export share the same record shape and
// query so an operator export and a daily archive cannot drift apart.
package feedbackexport

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
)

// Record is one feedback row. ID and AtMS support the scheduled export cursor
// and are deliberately absent from JSON; At is the human-readable UTC time.
type Record struct {
	ID              string `json:"-"`
	AtMS            int64  `json:"-"`
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

// Filter selects rows. Value is empty for all values. HasAfter applies the
// exclusive (at,id) cursor used by the scheduled export; the CLI does not set
// it. Results always use the same stable order.
type Filter struct {
	Project  string
	Value    string
	Since    int64
	AfterAt  int64
	AfterID  string
	HasAfter bool
}

// Queryer is satisfied by *sql.DB and *sql.Tx.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Query returns matching feedback rows ordered by (at,id). It performs no
// writes, so the CLI can use a read-only connection while the service runs.
func Query(ctx context.Context, q Queryer, f Filter) ([]Record, error) {
	query := "SELECT id,at,project,room,session,native_session,value,user,question,answer_summary,message FROM feedback WHERE at>=?"
	args := []any{f.Since}
	if f.Value != "" {
		query += " AND value=?"
		args = append(args, f.Value)
	}
	if f.Project != "" {
		query += " AND project=?"
		args = append(args, f.Project)
	}
	if f.HasAfter {
		query += " AND (at>? OR (at=? AND id>?))"
		args = append(args, f.AfterAt, f.AfterAt, f.AfterID)
	}
	query += " ORDER BY at,id"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		var record Record
		var native sql.NullString
		if err = rows.Scan(&record.ID, &record.AtMS, &record.Project, &record.Room, &record.Session, &native, &record.Value, &record.User, &record.Question, &record.AnswerSummary, &record.MessageID); err != nil {
			return nil, err
		}
		record.At = time.UnixMilli(record.AtMS).UTC().Format(time.RFC3339Nano)
		record.NativeSessionID = native.String
		record.QuestionMissing = record.Question == ""
		records = append(records, record)
	}
	return records, rows.Err()
}

// Encode writes one JSON object per line.
func Encode(w io.Writer, records []Record) error {
	encoder := json.NewEncoder(w)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

// ErrExists means today's archive already exists and was left untouched.
var ErrExists = errors.New("feedback export file already exists")

// Filename returns the documented daily archive name. Project IDs are
// configuration-validated; this also rejects path separators defensively.
func Filename(project string, day time.Time) (string, error) {
	if strings.ContainsAny(project, `/\`) || project == "" || project == "." || project == ".." {
		return "", fmt.Errorf("invalid feedback export project")
	}
	return "feedback-" + project + "-" + day.UTC().Format("2006-01-02") + ".jsonl", nil
}

// Write archives records as <dir>/feedback-<project>-<YYYY-MM-DD>.jsonl. The
// data is written to a 0600 temporary file in the same directory, synced and
// renamed, so a reader never sees a partial line. An existing destination is
// never overwritten; the caller decides how to retry on the next day.
func Write(dir, project string, day time.Time, records []Record) (string, error) {
	name, err := Filename(project, day)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0600); err != nil {
		return "", err
	}
	if err = Encode(tmp, records); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	if _, err = os.Lstat(path); err == nil {
		return "", ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err = os.Rename(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", ErrExists
		}
		return "", err
	}
	keep = true
	return path, nil
}
