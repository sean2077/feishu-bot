package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

const maintenanceHelp = `feishu-bot maintenance ack-uncertain [--config path] --before RFC3339|unix-ms
                               [--apply] [--json]

Inspect or acknowledge stuck uncertain records in the local state database.
The default is a read-only dry run: it lists how many inbox status=uncertain and
outbox state=uncertain|blocked records are older than --before, plus the oldest
time, bounded rooms and short sample IDs. --apply updates those records to
acknowledged in one transaction. Record bodies are never read. Use this only
after verifying the external result; acknowledgement is not a replay.
`

type ackUncertainReport struct {
	Mode       string                    `json:"mode"`
	Before     string                    `json:"before"`
	BeforeMS   int64                     `json:"beforeMs"`
	Candidates *store.UncertainSummary   `json:"candidates,omitempty"`
	Updated    *store.UncertainAckResult `json:"updated,omitempty"`
}

func runMaintenance(ctx context.Context, c core.Config, o Options, out io.Writer) error {
	if o.Subcommand != "ack-uncertain" {
		return errors.New("unknown maintenance command; use feishu-bot maintenance --help")
	}
	if !o.MaintenanceBeforeSet {
		return errors.New("maintenance ack-uncertain requires --before")
	}
	report := ackUncertainReport{
		Mode:     "dry-run",
		Before:   time.UnixMilli(o.MaintenanceBefore).UTC().Format(time.RFC3339),
		BeforeMS: o.MaintenanceBefore,
	}
	path := filepath.Join(c.DataDir, "gateway.sqlite")
	if o.Apply {
		st, err := store.OpenWritable(path)
		if err != nil {
			return fmt.Errorf("cannot open state database for --apply: %w", err)
		}
		defer st.Close()
		updated, err := st.AckUncertain(ctx, o.MaintenanceBefore)
		if err != nil {
			return err
		}
		report.Mode = "apply"
		report.Updated = &updated
	} else {
		st, err := store.OpenReadOnly(path)
		if err != nil {
			return fmt.Errorf("cannot open state database read-only: %w", err)
		}
		defer st.Close()
		candidates, err := st.UncertainSummary(ctx, "", &o.MaintenanceBefore)
		if err != nil {
			return err
		}
		report.Candidates = &candidates
	}
	if o.JSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	return writeUncertainReport(out, report)
}

func writeUncertainReport(out io.Writer, report ackUncertainReport) error {
	mode := "dry-run（只读，不写数据库）"
	if report.Mode == "apply" {
		mode = "apply（已写入）"
	}
	if _, err := fmt.Fprintf(out, "未确认记录确认：%s\n截止时间：%s（Unix 毫秒 %d；仅处理 at 早于该时间的记录）\n", mode, report.Before, report.BeforeMS); err != nil {
		return err
	}
	if report.Candidates != nil {
		for _, item := range []struct {
			label  string
			bucket store.UncertainBucket
		}{
			{"inbox · status=uncertain", report.Candidates.Inbox},
			{"outbox · state=uncertain|blocked", report.Candidates.Outbox},
		} {
			line := fmt.Sprintf("%s：%d 条", item.label, item.bucket.Count)
			if item.bucket.OldestAt != nil {
				line += " · 最早 " + time.UnixMilli(*item.bucket.OldestAt).UTC().Format(time.RFC3339)
			}
			line += fmt.Sprintf(" · %d 个房间", item.bucket.RoomCount)
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
			if len(item.bucket.Rooms) > 0 {
				rooms := strings.Join(item.bucket.Rooms, ", ")
				if item.bucket.RoomCount > len(item.bucket.Rooms) {
					rooms += fmt.Sprintf(" 等 %d 个", item.bucket.RoomCount)
				}
				if _, err := fmt.Fprintln(out, "  房间："+rooms); err != nil {
					return err
				}
			}
			if len(item.bucket.Samples) > 0 {
				if _, err := fmt.Fprintln(out, "  样本："+strings.Join(item.bucket.Samples, ", ")); err != nil {
					return err
				}
			}
		}
		total := report.Candidates.Inbox.Count + report.Candidates.Outbox.Count
		_, err := fmt.Fprintf(out, "将标记 %d 条记录为 acknowledged；dry-run 未写入数据库，加 --apply 才会更新。\n", total)
		return err
	}
	if report.Updated != nil {
		_, err := fmt.Fprintf(out, "已更新为 acknowledged：inbox %d 条，outbox %d 条。\n", report.Updated.Inbox, report.Updated.Outbox)
		return err
	}
	return errors.New("maintenance report has no candidates or update result")
}
