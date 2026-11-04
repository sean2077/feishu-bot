package gateway

import (
	"errors"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/workspace"
	"os"
	"path/filepath"
)

func (g *Gateway) request(s *slot, ev core.WorkerEvent) (core.Args, error) {
	current, ok := RoomFromKey(g.config, s.room.Key)
	if !ok || s.ctx.Err() != nil {
		return nil, errors.New("conversation is no longer authorized")
	}
	project, _ := g.config.Project(current.ProjectID)
	switch ev.Kind {
	case "history":
		return g.history(s.ctx, current, s.session.ID, ev.Input)
	case "action":
		return g.executeAction(s.ctx, current, s.session.ID, project, ev.Name, ev.Input, s.workspace)
	case "attachment":
		id, key := stringArg(ev.Input, "messageId"), stringArg(ev.Input, "key")
		message, e := g.store.ContextMessage(current.Key, id)
		if e != nil {
			return nil, e
		}
		if message == nil || !Readable(current, *message) {
			return nil, errors.New("attachment message is not authorized in this conversation")
		}
		var resource *core.Resource
		for _, r := range message.Resources {
			if r.Key == key {
				v := r
				resource = &v
				break
			}
		}
		if resource == nil {
			return nil, errors.New("unknown attachment")
		}
		name := core.Hash(id + key)[:24] + "-" + safeAttachmentName(resource.Name)
		path := filepath.Join(s.workspace.Inbound, name)
		e = providerVoid(s.ctx, func() error {
			err := g.transport.Download(s.ctx, id, *resource, path, g.config.Limits.AttachmentBytes)
			if s.ctx.Err() != nil {
				_ = os.Remove(path)
				return s.ctx.Err()
			}
			return err
		})
		if e != nil {
			return nil, e
		}
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
		return core.Args{"ok": true, "path": path}, nil
	case "send_file":
		root, path, e := workspace.TranslateOutput(s.workspace, stringArg(ev.Input, "path"))
		if e != nil {
			return nil, e
		}
		snapshot := filepath.Join(g.config.DataDir, "exports", core.Hash(current.Key)[:32], core.NewID())
		if e = workspace.Snapshot(root, path, snapshot, g.config.Limits.AttachmentBytes); e != nil {
			return nil, e
		}
		recorded := false
		defer func() {
			if !recorded {
				_ = os.Remove(snapshot)
			}
		}()
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
		name := stringArg(ev.Input, "name")
		if name == "" {
			name = filepath.Base(path)
		}
		name = safeAttachmentName(name)
		s.mu.Lock()
		who := []core.Origin{}
		for _, d := range s.deliveries {
			who = append(who, core.Origin{ID: d.Message.ID, SenderID: d.Message.SenderID})
		}
		reply := ""
		if d, ok := s.deliveries[s.anchor]; ok {
			reply = d.Message.ID
		}
		s.mu.Unlock()
		id, e := g.saveOutput(g.store.DB, current, s.session.ID, "", true, "", reply, "", nil, who, name, snapshot, "file", "")
		if e != nil {
			return nil, e
		}
		recorded = true
		if s.ctx.Err() != nil {
			_, _ = g.store.DB.Exec("UPDATE outbox SET state='cancelled' WHERE id=?", id)
			return nil, s.ctx.Err()
		}
		sent, e := g.deliverOutput(s.ctx, id, current, false)
		if e != nil || !sent {
			return core.Args{"ok": false, "deliveryId": id, "error": g.outputError(id)}, nil
		}
		o, e := g.output(id)
		if e != nil {
			return nil, e
		}
		return core.Args{"ok": true, "deliveryId": id, "messageId": o.FileTarget}, nil
	default:
		var questions []question
		if ev.Kind == "question" {
			var valid bool
			questions, valid = parseQuestions(ev.Input["questions"])
			if !valid {
				return core.Args{"approved": false, "reason": "unsupported question shape"}, nil
			}
		}
		return g.ask(s.ctx, current, s.session.ID, ev.Name, ev.Input, "工具："+ev.Name+"\n参数：`"+cut(jsonText(g.redactArgs(ev.Input)), 5000)+"`", questions)
	}
}
