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
		result, access, e := g.historyRequest(s.ctx, current, s.session.ID, ev.Input, g.requestContext(s))
		if e != nil {
			return nil, e
		}
		if access.CrossChat {
			s.markTainted(access)
		}
		return result, nil
	case "action":
		if s.taintCheck() {
			return g.executeActionTainted(s.ctx, current, s.session.ID, project, ev.Name, ev.Input, s.workspace, g.requestContext(s), true, s.taintTargetsSnapshot())
		}
		return g.executeAction(s.ctx, current, s.session.ID, project, ev.Name, ev.Input, s.workspace, g.requestContext(s))
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
		path, e := g.downloadAttachment(s.ctx, s.workspace, id, *resource)
		if e != nil {
			return nil, e
		}
		return core.Args{"ok": true, "path": path, "name": resource.Name}, nil
	case "send_file":
		if s.taintCheck() {
			requestContext := g.requestContext(s)
			policy := taintedApprovalPolicy(approvalPolicyForTool(project, requestContext), current.DMUser, s.taintTargetsSnapshot())
			summary := "本轮读取过其他群的内容；发送文件需要你确认。\n" + requesterLine(requestContext) + "\n参数：`" + cut(jsonText(g.redactArgs(ev.Input)), 2000) + "`"
			decision, e := g.ask(s.ctx, current, s.session.ID, "send_file", ev.Input, summary, nil, policy)
			if e != nil {
				return nil, e
			}
			if !boolArg(decision, "approved") {
				return core.Args{"ok": false, "error": "operation not approved"}, nil
			}
		}
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
			reply = messageReply(d.Message)
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
		requestContext := g.requestContext(s)
		policy := approvalPolicyForTool(project, requestContext)
		summary := "工具：" + ev.Name + "\n" + requesterLine(requestContext) + "\n参数：`" + cut(jsonText(g.redactArgs(ev.Input)), 5000) + "`"
		if ev.Tainted {
			policy = taintedApprovalPolicy(policy, current.DMUser, s.taintTargetsSnapshot())
			summary = "本轮读取过其他群的内容；只读工具已放行，写操作需要你确认。\n" + summary
		}
		return g.ask(s.ctx, current, s.session.ID, ev.Name, ev.Input, summary, questions, policy)
	}
}
