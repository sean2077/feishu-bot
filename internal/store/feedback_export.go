package store

import (
	"context"
	"database/sql"
	"errors"
)

// FeedbackExportMark is the last row committed to a scheduled archive. The
// (At,ID) pair is the export cursor; File records which day's archive covered
// it so a second sweep does not overwrite the same day's file.
type FeedbackExportMark struct {
	At      int64
	ID      string
	File    string
	Rows    int64
	Updated int64
}

func (s *Store) FeedbackExportMark(ctx context.Context, project string) (FeedbackExportMark, bool, error) {
	var mark FeedbackExportMark
	err := s.DB.QueryRowContext(ctx, "SELECT at,id,file,rows,updated FROM feedback_exports WHERE project=?", project).
		Scan(&mark.At, &mark.ID, &mark.File, &mark.Rows, &mark.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return mark, false, nil
	}
	return mark, err == nil, err
}

func (s *Store) SaveFeedbackExport(ctx context.Context, project string, mark FeedbackExportMark) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO feedback_exports(project,at,id,file,rows,updated) VALUES(?,?,?,?,?,?)
ON CONFLICT(project) DO UPDATE SET at=excluded.at,id=excluded.id,file=excluded.file,rows=excluded.rows,updated=excluded.updated`,
		project, mark.At, mark.ID, mark.File, mark.Rows, mark.Updated)
	return err
}

// FeedbackProjects returns projects that currently have feedback rows. It does
// not depend on current routing configuration, so feedback from a removed
// project is still archived before retention deletes it.
func (s *Store) FeedbackProjects(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT DISTINCT project FROM feedback ORDER BY project")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []string
	for rows.Next() {
		var project string
		if err = rows.Scan(&project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}
