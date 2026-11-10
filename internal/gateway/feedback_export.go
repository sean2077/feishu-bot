package gateway

import (
	"context"
	"path/filepath"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/feedbackexport"
	"github.com/sean2077/feishu-bot/internal/store"
)

// exportFeedback archives feedback into day-scoped per-project files before
// maintenance may delete it. It runs at most once per project per UTC day: the
// saved file path is the day marker, and a second sweep leaves that file
// untouched. A failure returns the feedback retention policy as paused so the
// sweep cannot remove unexported rows.
func (g *Gateway) exportFeedback(ctx context.Context, now int64) []string {
	if !g.config.Feedback.Export.Enabled {
		return nil
	}
	day := time.UnixMilli(now).UTC()
	projects, err := g.store.FeedbackProjects(ctx)
	if err != nil {
		g.feedbackExportFailure("", day, err)
		return []string{"feedback"}
	}
	paused := false
	for _, project := range projects {
		if ctx.Err() != nil {
			return []string{"feedback"}
		}
		mark, saved, err := g.store.FeedbackExportMark(ctx, project)
		if err != nil {
			g.feedbackExportFailure(project, day, err)
			paused = true
			continue
		}
		name, err := feedbackexport.Filename(project, day)
		if err != nil {
			g.feedbackExportFailure(project, day, err)
			paused = true
			continue
		}
		path := filepath.Join(g.config.Feedback.Export.Dir, name)
		if saved && mark.File == path {
			continue
		}
		records, err := feedbackexport.Query(ctx, g.store.DB, feedbackexport.Filter{
			Project: project, AfterAt: mark.At, AfterID: mark.ID, HasAfter: saved,
		})
		if err != nil {
			g.feedbackExportFailure(project, day, err)
			paused = true
			continue
		}
		if len(records) == 0 {
			continue
		}
		if _, err = feedbackexport.Write(g.config.Feedback.Export.Dir, project, day, records); err != nil {
			g.feedbackExportFailure(project, day, err)
			paused = true
			continue
		}
		last := records[len(records)-1]
		if err = g.store.SaveFeedbackExport(ctx, project, store.FeedbackExportMark{
			At: last.AtMS, ID: last.ID, File: path, Rows: int64(len(records)), Updated: now,
		}); err != nil {
			g.feedbackExportFailure(project, day, err)
			paused = true
			continue
		}
		delete(g.exportFailures, project)
	}
	if paused {
		return []string{"feedback"}
	}
	return nil
}

// feedbackExportFailure audits one failure per project, day and error text so
// a repeated 30-second sweep does not flood the audit table.
func (g *Gateway) feedbackExportFailure(project string, day time.Time, err error) {
	if g.exportFailures == nil {
		g.exportFailures = map[string]string{}
	}
	key := day.Format("2006-01-02") + "|" + err.Error()
	if g.exportFailures[project] == key {
		return
	}
	g.exportFailures[project] = key
	g.audit("", "feedback_export_failed", core.Args{"project": project, "date": day.Format("2006-01-02"), "error": err.Error()})
}
