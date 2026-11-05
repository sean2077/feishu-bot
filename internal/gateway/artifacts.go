package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

func (g *Gateway) artifactRoot(r core.Room) string {
	return filepath.Join(g.config.DataDir, "exports", core.Hash(r.Key)[:32])
}

func (g *Gateway) availableArtifact(r core.Room, o outgoing) bool {
	if o.Room != r.Key || o.State != "done" || o.FileState != "done" || o.FileTarget == "" || o.FileName == "" || o.FilePath == "" || !core.Inside(g.artifactRoot(r), o.FilePath) || !g.outputAllowed(o, r) {
		return false
	}
	info, err := os.Stat(o.FilePath)
	return err == nil && info.Mode().IsRegular() && info.Size() <= g.config.Limits.AttachmentBytes
}

func (g *Gateway) artifactsView(r core.Room, asJSON bool) (string, error) {
	rows, err := g.store.DB.Query(`SELECT id,at FROM outbox WHERE room=? AND state='done' AND file_state='done' AND file_target IS NOT NULL AND file_name IS NOT NULL ORDER BY at DESC,id DESC LIMIT 20`, r.Key)
	if err != nil {
		return "", err
	}
	type item struct {
		id string
		at int64
	}
	list := []item{}
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.at); err != nil {
			rows.Close()
			return "", err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	items := []core.Args{}
	lines := []string{"**本聊天已完成文件**", "显示最近 20 条。文件保留期内可直接重新发送；不会重新执行原任务。"}
	for _, v := range list {
		o, err := g.output(v.id)
		if err != nil {
			return "", err
		}
		if !g.outputAllowed(o, r) {
			continue
		}
		available := g.availableArtifact(r, o)
		items = append(items, core.Args{"id": o.ID, "name": o.FileName, "session": o.Session, "createdAt": v.at, "available": available})
		line := "\n" + o.FileName + " · " + timeLabel(v.at) + "\n`" + o.ID + "`"
		if available {
			line += "\n取回：`/artifacts " + o.ID + "`"
		} else {
			line += "\n副本已过保留期或不可用；原飞书附件仍可从历史消息查找。"
		}
		lines = append(lines, line)
	}
	if asJSON {
		return jsonPretty(items), nil
	}
	if len(items) == 0 {
		return "本聊天没有可列出的已完成文件。待交付文件请用 /outputs 查看。", nil
	}
	return strings.Join(lines, "\n"), nil
}

func (g *Gateway) artifactCommand(ctx context.Context, r core.Room, m core.Message, arg string) error {
	if !AuthorizedAuthor(r, m.SenderID) {
		return nil
	}
	if arg == "" || arg == "--json" {
		text, err := g.artifactsView(r, arg == "--json")
		if err != nil {
			return err
		}
		return g.notice(ctx, r, text, &m)
	}
	o, err := g.output(arg)
	if err != nil || !g.availableArtifact(r, o) {
		g.notify(r, "无法取回：文件不属于本聊天、尚未完成或副本已不可用。用 /artifacts 查看当前列表。", &m)
		return nil
	}
	// Copy the frozen export, never resolve the original workspace path. The
	// rooted open in Snapshot also rejects symlinks escaping this room's exports.
	snapshot := filepath.Join(g.artifactRoot(r), core.NewID())
	if err := workspace.Snapshot(g.artifactRoot(r), o.FilePath, snapshot, g.config.Limits.AttachmentBytes); err != nil {
		g.notify(r, "保存的文件副本已不可用；请从原飞书消息取回。", &m)
		return nil
	}
	id, err := g.saveOutput(g.store.DB, r, o.Session, "", true, "", m.ID, m.ThreadID, nil, append(o.Origins, origin(m)...), o.FileName, snapshot, "file", "pending")
	if err != nil {
		_ = os.Remove(snapshot)
		return err
	}
	ok, err := g.deliverOutput(ctx, id, r, false)
	if !ok {
		g.notify(r, "文件重新发送尚未完成。用 /outputs 查看交付记录；无需重新运行模型。", &m)
	}
	return err
}
