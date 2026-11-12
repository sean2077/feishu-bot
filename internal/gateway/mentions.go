package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

const mentionBodyLimit = 2000
const mentionSummaryLimit = 200

func matchOtherBot(bots []core.OtherBot, target string) (core.OtherBot, bool) {
	target = strings.TrimSpace(target)
	for _, b := range bots {
		if target == b.AppID || strings.EqualFold(target, b.Name) {
			return b, true
		}
	}
	return core.OtherBot{}, false
}

// mentionAdmins requires the whole active sender set to be chat maintainers.
// WP50 can later replace this single admin-identity check with its group
// administrator list. A missing request context is rejected.
func mentionAdmins(r core.Room, request *core.RequestContext) bool {
	if request == nil || len(request.Senders) == 0 || len(r.Maintainers) == 0 {
		return false
	}
	for _, sender := range request.Senders {
		if !contains(r.Maintainers, sender) {
			return false
		}
	}
	return true
}

func (g *Gateway) mentionBot(ctx context.Context, s *slot, room core.Room, p core.Project, input core.Args) (core.Args, error) {
	request := g.requestContext(s)
	body := stringArg(input, "body")
	if len([]rune(body)) > mentionBodyLimit {
		body = cut(body, mentionBodyLimit)
	}
	target := strings.TrimSpace(stringArg(input, "target"))
	if !room.AllowOtherBots || len(room.OtherBots) == 0 {
		g.auditMention(room, request, cut(target, 80), body, "disabled")
		return nil, errors.New("mention_bot is not enabled in this group")
	}
	bot, ok := matchOtherBot(room.OtherBots, target)
	if !ok || bot.OpenID == "" {
		g.auditMention(room, request, cut(target, 80), body, "denied:target")
		return nil, errors.New("target bot is not in the configured allowlist or has no openId")
	}
	if !mentionAdmins(room, request) {
		g.auditMention(room, request, bot.AppID, body, "denied:admin")
		return nil, errors.New("mention_bot requires every active sender to be a chat maintainer")
	}
	if !s.claimMention() {
		g.auditMention(room, request, bot.AppID, body, "denied:per_turn")
		return nil, errors.New("mention_bot allows at most one target per turn")
	}
	if s.taintCheck() {
		policy := taintedApprovalPolicy(approvalPolicyForTool(p, request), room.DMUser, s.taintTargetsSnapshot())
		summary := "本轮读取过其他群的内容；@ 其他机器人需要你确认。\n" + requesterLine(request) + "\n目标：" + bot.Name
		if body != "" {
			summary += "\n正文：" + cut(body, 500)
		}
		decision, err := g.ask(ctx, room, s.session.ID, "mention_bot", input, summary, nil, policy)
		if err != nil {
			return nil, err
		}
		if !boolArg(decision, "approved") {
			g.auditMention(room, request, bot.AppID, body, "denied:approval")
			return nil, errors.New("mention_bot was not approved")
		}
	}
	elements := []any{core.Args{"tag": "at", "user_id": bot.OpenID, "user_name": bot.Name}}
	if body != "" {
		elements = append(elements, core.Args{"tag": "text", "text": " " + body})
	}
	content := core.Args{"zh_cn": core.Args{"content": []any{elements}}}
	reply := ""
	if request != nil {
		reply = request.MessageID
	}
	thread := ""
	if request != nil {
		thread = request.ThreadID
	}
	messageID, err := g.transport.SendPost(ctx, room, content, reply, thread)
	if err != nil {
		g.auditMention(room, request, bot.AppID, body, "failed")
		return nil, err
	}
	text := "@" + bot.Name
	if body != "" {
		text += " " + body
	}
	if _, err = g.saveOutput(g.store.DB, room, s.session.ID, text, true, messageID, reply, thread, nil, nil, "", "", "mention", "done"); err != nil {
		g.audit(room.Key, "mention_bot_record_failed", core.Args{"target": bot.AppID, "error": core.Redact(err.Error(), g.secrets)})
		return core.Args{"ok": true, "target": bot.Name, "messageId": messageID, "warning": "message sent but not recorded; do not retry"}, nil
	}
	g.auditMention(room, request, bot.AppID, body, "sent")
	return core.Args{"ok": true, "target": bot.Name, "messageId": messageID, "hasBody": body != ""}, nil
}

func (g *Gateway) auditMention(room core.Room, request *core.RequestContext, target, body, result string) {
	thread := ""
	if request != nil {
		thread = request.ThreadID
	}
	g.audit(room.Key, "mention_bot", core.Args{
		"requester": requesterID(request),
		"senders":   sendersOf(request),
		"chat":      room.ChatID,
		"thread":    thread,
		"target":    target,
		"hasBody":   body != "",
		"body":      cut(body, mentionSummaryLimit),
		"result":    result,
	})
}

func requesterID(request *core.RequestContext) string {
	if request == nil {
		return ""
	}
	return request.SenderOpenID
}

func sendersOf(request *core.RequestContext) []string {
	if request == nil {
		return nil
	}
	return request.Senders
}
