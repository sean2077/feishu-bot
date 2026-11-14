package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

func notifyError(code, message string) *core.AdoptError {
	return &core.AdoptError{Code: code, Message: message}
}

func notifyFailure(code, message string) core.NotifyResult {
	return core.NotifyResult{OK: false, Error: notifyError(code, message)}
}

func notifyBody(title, summary, footer string) string {
	var text string
	switch {
	case title != "" && summary != "":
		text = "**" + title + "**\n\n" + summary
	case summary != "":
		text = summary
	default:
		text = title
	}
	if footer != "" {
		text += "\n\n— " + footer
	}
	return text
}

func (g *Gateway) notifyValidation(req core.NotifyRequest) (core.Project, *core.AdoptError) {
	if req.ID == "" || len(req.ID) > 64 {
		return core.Project{}, notifyError(core.NotifyErrorInvalidRequest, "notify 请求缺少有效 ID")
	}
	if req.At > 0 && time.Since(time.UnixMilli(req.At)) > adoptRequestTTL {
		return core.Project{}, notifyError(core.NotifyErrorInvalidRequest, "notify 请求已过期，请重新发起")
	}
	if !core.ValidThreadRoot(req.Native) {
		return core.Project{}, notifyError(core.NotifyErrorInvalidRequest, "本地会话 ID 无效")
	}
	if req.HolderPID < 0 || req.HolderStarted < 0 {
		return core.Project{}, notifyError(core.NotifyErrorInvalidRequest, "本地终端进程信息无效")
	}
	dm, ok := g.config.DM(req.User)
	if !ok || !g.config.AdoptEnabled(req.User) {
		return core.Project{}, notifyError(core.NotifyErrorNotConfigured, "该私聊未启用本地会话认领")
	}
	project, ok := g.config.Project(dm.ProjectID)
	if !ok || project.NativeConfig != "inherit" {
		return core.Project{}, notifyError(core.NotifyErrorNotConfigured, "notify 要求项目的 nativeConfig 为 inherit")
	}
	return project, nil
}

func (g *Gateway) notifyWork(req core.NotifyRequest, expected string) (string, string, *core.AdoptError) {
	_, configDir, _, err := workspace.InheritedConfig()
	if err != nil {
		return "", "", notifyError(core.NotifyErrorNotConfigured, "网关的继承 Claude 配置目录不可用")
	}
	transcript, err := native.LocateTranscript(configDir, req.Native)
	if err != nil {
		return "", "", notifyError(core.NotifyErrorInvalidRequest, err.Error())
	}
	work, err := workspace.PhysicalDirectory(transcript.CWD)
	if err != nil {
		return "", "", notifyError(core.NotifyErrorInvalidRequest, "本地会话工作目录不可用")
	}
	if !native.SameProjectDir(native.EncodeProjectDir(work), filepath.Base(filepath.Dir(transcript.Path))) {
		return "", "", notifyError(core.NotifyErrorInvalidRequest, "本地会话的物理工作目录与 transcript 项目目录不一致（符号链接或大小写差异）")
	}
	if expected != "" && !core.SamePath(work, expected) {
		return "", "", notifyError(core.NotifyErrorInvalidRequest, "本地会话工作目录与认领记录不一致")
	}
	if !g.config.AdoptWorkAllowed(req.User, work) {
		return "", "", notifyError(core.NotifyErrorInvalidRequest, "本地会话工作目录不在 adoptRoots 内")
	}
	return work, transcript.Model, nil
}

func existingNotifyResult(record store.AdoptedTopic) core.NotifyResult {
	result := core.NotifyResult{
		OK:            false,
		State:         record.State,
		ChatID:        adoptedChatID(record.Room),
		RootMessageID: record.Root,
		ThreadID:      record.Thread,
		SessionName:   record.SessionName,
		TopicURL:      topicAppLink(adoptedChatID(record.Room), record.Thread),
		Created:       false,
	}
	switch record.State {
	case store.AdoptedOpen:
		result.OK = true
	case store.AdoptedPending:
		result.Error = notifyError(core.NotifyErrorPending, "本地会话认领尚未完成；不会重发")
	case store.AdoptedUncertain:
		result.Error = notifyError(core.NotifyErrorUncertain, "本地会话认领结果未确认；不会重发")
	default:
		result.Error = notifyError(core.NotifyErrorInvalidRequest, "本地会话认领状态无效")
	}
	return result
}

func (g *Gateway) Notify(ctx context.Context, req core.NotifyRequest) core.NotifyResult {
	if !g.beginCall() {
		return notifyFailure(core.NotifyErrorGatewayUnavailable, "网关正在关闭")
	}
	defer g.wg.Done()
	if _, bad := g.notifyValidation(req); bad != nil {
		return core.NotifyResult{OK: false, Error: bad}
	}
	title := strings.Join(strings.Fields(cleanAttachText(req.Title, core.AdoptTitleRunes)), " ")
	summary := cleanAttachText(req.Summary, core.AdoptSummaryRunes)
	if summary == "" {
		summary = title
	}
	if summary == "" {
		return notifyFailure(core.NotifyErrorInvalidRequest, "notify 需要摘要正文")
	}
	unlock, err := g.adoptLocks.lockContext(ctx, req.Native)
	if err != nil {
		return notifyFailure(core.NotifyErrorInvalidRequest, err.Error())
	}
	defer unlock()
	record, found, err := g.store.AdoptedTopicByNative(req.Native)
	if err != nil {
		return notifyFailure(core.NotifyErrorInvalidRequest, "认领记录读取失败")
	}
	if found {
		if record.User != req.User {
			return notifyFailure(core.NotifyErrorInvalidRequest, "该原生会话属于其他私聊，不能在此通知")
		}
		if record.State != store.AdoptedOpen {
			return existingNotifyResult(record)
		}
		if _, _, bad := g.notifyWork(req, record.Work); bad != nil {
			return core.NotifyResult{OK: false, Error: bad}
		}
		current, currentFound, currentErr := g.store.CurrentSession(record.Room)
		if currentErr != nil {
			return notifyFailure(core.NotifyErrorInvalidRequest, "当前会话读取失败")
		}
		sameSession := currentFound && current.ID == record.Session
		if sameSession {
			if record.HolderPID > 0 && g.holderAlive(record) &&
				(record.HolderPID != req.HolderPID || record.HolderStarted != req.HolderStarted) {
				return notifyFailure(core.NotifyErrorInvalidRequest, "该会话仍由另一个本地终端持有")
			}
			if req.HolderPID > 0 {
				_ = g.store.UpdateAdoptionHolder(record.Native, req.HolderPID, req.HolderStarted)
			}
		} else {
			g.clearAdoptEndpoint(record.Room)
		}
		room, ok := g.config.AdoptedTopicRoom(adoptedChatID(record.Room), req.User, record.Root)
		if !ok || room.Key != record.Room {
			return notifyFailure(core.NotifyErrorInvalidRequest, "认领话题配置已失效")
		}
		name := chosenAdoptName(req.SessionName, record.SessionName, record.Native)
		warnings := g.syncAdoptionName(ctx, &record, req.SessionName)
		messageID, err := g.transport.Send(ctx, room, notifyBody(title, summary, adoptFooter(name, record.Native)), room.Thread, "")
		if err != nil {
			g.audit(room.Key, "notify_failed", core.Args{"native": req.Native, "uncertain": deliveryUncertain(err)})
			return core.NotifyResult{
				OK:            false,
				State:         store.AdoptedOpen,
				ChatID:        room.ChatID,
				RootMessageID: record.Root,
				ThreadID:      record.Thread,
				SessionName:   name,
				TopicURL:      topicAppLink(room.ChatID, record.Thread),
				Created:       false,
				Warnings:      warnings,
				Error:         notifyError(core.NotifyErrorTransportFailure, "通知写入话题失败"),
			}
		}
		if sameSession {
			g.setAdoptEndpoint(room.Key, adoptEndpoint{Socket: req.MessagingSocket, Token: req.MessagingToken, ConfigPath: req.ConfigPath, Native: req.Native})
		}
		g.audit(room.Key, "notify", core.Args{"native": req.Native, "message": messageID, "created": false})
		return core.NotifyResult{OK: true, State: store.AdoptedOpen, ChatID: room.ChatID, RootMessageID: record.Root, ThreadID: record.Thread, MessageID: messageID, SessionName: name, TopicURL: topicAppLink(room.ChatID, record.Thread), Created: false, Warnings: warnings}
	}
	return g.notifyCreate(ctx, req, title, summary)
}

func (g *Gateway) notifyCreate(ctx context.Context, req core.NotifyRequest, title, summary string) core.NotifyResult {
	work, model, bad := g.notifyWork(req, "")
	if bad != nil {
		return core.NotifyResult{OK: false, Error: bad}
	}
	dm, _ := g.config.DM(req.User)
	project, _ := g.config.Project(dm.ProjectID)
	name := adoptDisplayName(req.SessionName, req.Native)
	warnings := []string{}
	if warning := adoptedModelWarning(g.gatewayModel(project), model); warning != "" {
		warnings = append(warnings, warning)
	}
	record, created, err := g.store.BeginAdoption(req.Native, req.User, work, req.HolderPID, req.HolderStarted)
	if err != nil {
		if errors.Is(err, store.ErrNativeClaimed) {
			return notifyFailure(core.NotifyErrorInvalidRequest, "该原生会话已被网关会话占用")
		}
		return notifyFailure(core.NotifyErrorInvalidRequest, "认领意图写入失败")
	}
	if !created {
		return existingNotifyResult(record)
	}
	if err = g.store.SetAdoptionIdentity(req.Native, name, title); err != nil {
		_ = g.store.DeleteUnsentAdoption(req.Native)
		return notifyFailure(core.NotifyErrorInvalidRequest, "认领意图写入失败")
	}
	sender, ok := g.transport.(core.UserCardSender)
	if !ok {
		_ = g.store.DeleteUnsentAdoption(req.Native)
		return notifyFailure(core.NotifyErrorTransportFailure, "当前传输不支持认领卡片")
	}
	rootID, chatID, err := sender.SendUserCard(core.WithDeliveryID(ctx, "notify:"+req.ID+":root"), req.User, attachCard(name, title, work, req.Native))
	recordEvidence := func() {
		if rootID == "" || chatID == "" {
			return
		}
		if room, ok := g.config.AdoptedTopicRoom(chatID, req.User, rootID); ok {
			_ = g.store.SetAdoptionRootEvidence(req.Native, room.Key, rootID)
		}
	}
	if err != nil {
		if deliveryUncertain(err) {
			recordEvidence()
			_ = g.store.MarkAdoptionUncertain(req.Native)
			return core.NotifyResult{OK: false, State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: notifyError(core.NotifyErrorTransportFailure, "根卡片发送结果未确认；请检查私聊中的根消息，不会自动重发")}
		}
		_ = g.store.DeleteUnsentAdoption(req.Native)
		return notifyFailure(core.NotifyErrorTransportFailure, "根卡片发送失败")
	}
	room, ok := g.config.AdoptedTopicRoom(chatID, req.User, rootID)
	if !ok {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.NotifyResult{OK: false, State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: notifyError(core.NotifyErrorTransportFailure, "根卡片已发送，但话题房间配置校验失败")}
	}
	_ = g.store.SetAdoptionRootEvidence(req.Native, room.Key, rootID)
	_, configDir, override, configErr := workspace.InheritedConfig()
	if configErr != nil {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.NotifyResult{OK: false, State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: notifyError(core.NotifyErrorNotConfigured, "继承 Claude 配置目录不可用")}
	}
	sessionID := core.NewID()
	now := time.Now().UnixMilli()
	if err = g.store.BindAdoptionRoot(req.Native, room.Key, rootID, sessionID, configDir, override, now); err != nil {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.NotifyResult{OK: false, State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: notifyError(core.NotifyErrorTransportFailure, "根卡片已发送，但会话记录写入失败")}
	}
	replyID, err := g.transport.Send(core.WithDeliveryID(ctx, "notify:"+req.ID+":thread"), room, summary, rootID, "")
	if err != nil {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.NotifyResult{OK: false, State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: notifyError(core.NotifyErrorTransportFailure, "根卡片已发送，但话题回复失败")}
	}
	message, err := g.transport.Fetch(ctx, replyID, room)
	if err != nil || message == nil || message.ThreadID == "" || message.RootID != rootID {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.NotifyResult{OK: false, State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: notifyError(core.NotifyErrorTransportFailure, "根卡片已发送，但话题 ID 未能确认")}
	}
	if err = g.store.OpenAdoption(req.Native, message.ThreadID); err != nil {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.NotifyResult{OK: false, State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, ThreadID: message.ThreadID, SessionName: name, TopicURL: topicAppLink(chatID, message.ThreadID), Error: notifyError(core.NotifyErrorTransportFailure, "根卡片已发送且话题已建立，但状态写入失败")}
	}
	g.setAdoptEndpoint(room.Key, adoptEndpoint{Socket: req.MessagingSocket, Token: req.MessagingToken, ConfigPath: req.ConfigPath, Native: req.Native})
	g.audit(room.Key, "notify", core.Args{"native": req.Native, "root": rootID, "thread": message.ThreadID, "message": replyID, "created": true})
	return core.NotifyResult{OK: true, State: store.AdoptedOpen, ChatID: chatID, RootMessageID: rootID, ThreadID: message.ThreadID, MessageID: replyID, SessionName: name, TopicURL: topicAppLink(chatID, message.ThreadID), Created: true, Warnings: warnings}
}

func (g *Gateway) TopicReply(ctx context.Context, req core.TopicReplyRequest) core.TopicReplyResult {
	if !g.beginCall() {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorGatewayUnavailable, "网关正在关闭")}
	}
	defer g.wg.Done()
	if req.ID == "" || !core.ValidThreadRoot(req.Native) || req.MessageID == "" {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorInvalidRequest, "topic reply 参数无效")}
	}
	text := strings.TrimSpace(cleanAttachText(req.Text, g.config.Limits.TextChars))
	if text == "" {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorInvalidRequest, "回复正文为空")}
	}
	record, found, err := g.store.AdoptedTopicByNative(req.Native)
	if err != nil || !found || record.State != store.AdoptedOpen {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorInvalidRequest, "没有处于 open 状态的认领话题")}
	}
	room, ok := g.config.AdoptedTopicRoom(adoptedChatID(record.Room), record.User, record.Root)
	if !ok || room.Key != record.Room {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorNotConfigured, "认领话题配置已失效")}
	}
	message, status, found, err := g.store.Forwarded(room.Key, req.MessageID)
	if err != nil || !found {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorInvalidRequest, "该消息不是本话题已转递的输入")}
	}
	if status == "done" {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorInvalidRequest, "该消息已经回复")}
	}
	if status != store.InputForwarded && status != "uncertain" {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorInvalidRequest, "该消息没有成功转递给本机会话")}
	}
	replyFooter := "\n\n— " + adoptFooter(record.SessionName, record.Native)
	replyText := cut(text, g.config.Limits.TextChars-utf8.RuneCountInString(replyFooter)) + replyFooter
	replyID, err := g.transport.Send(ctx, room, replyText, room.Thread, "")
	if err != nil {
		if deliveryUncertain(err) {
			_, _ = g.store.MarkForwarded(room.Key, req.MessageID, "uncertain")
			g.audit(room.Key, "topic_reply_failed", core.Args{"message": req.MessageID, "native": req.Native, "uncertain": true})
		}
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorTransportFailure, "回复写入话题失败")}
	}
	if _, err = g.store.MarkForwarded(room.Key, req.MessageID, "done"); err != nil {
		return core.TopicReplyResult{Error: notifyError(core.NotifyErrorTransportFailure, "回复已发送，但状态写入失败")}
	}
	g.settleReceipts(room.Key, record.Session, origin(message), true)
	g.audit(room.Key, "topic_reply", core.Args{"message": req.MessageID, "native": req.Native, "reply": replyID})
	return core.TopicReplyResult{OK: true, MessageID: replyID}
}
