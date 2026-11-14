package gateway

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

const adoptRequestTTL = 5 * time.Minute

// Attach adopts one running local Claude Code session into a Feishu DM topic.
// It persists intent before the first external write and never replays an
// unconfirmed write. Work is derived from the transcript below the gateway's
// inherited Claude configuration directory, never from the caller's shell.
func (g *Gateway) Attach(ctx context.Context, req core.AttachRequest) core.AttachResult {
	if !g.beginCall() {
		return core.AttachResult{Error: "网关正在关闭，未处理认领请求"}
	}
	defer g.wg.Done()
	result, err := g.attach(ctx, req)
	if err != nil {
		result.OK = false
		if result.Error == "" {
			result.Error = core.Redact(err.Error(), g.secrets)
		}
	}
	return result
}

func deliveryUncertain(err error) bool {
	var failure core.DeliveryFailure
	return errors.As(err, &failure) && failure.DeliveryUncertain()
}

func (g *Gateway) gatewayModel(project core.Project) string {
	if project.ModelProfile != "" && project.ModelProfile != "default" {
		if profile, ok := g.config.ModelProfiles[project.ModelProfile]; ok && profile.Model != "" {
			return profile.Model
		}
	}
	return g.config.Model.Model
}

// adoptDisplayName returns a bounded display name for a local session. It
// falls back to the first eight native ID characters when the caller has none.
func adoptDisplayName(name, nativeID string) string {
	name = strings.Join(strings.Fields(cleanAttachText(name, core.AdoptNameRunes)), " ")
	if name == "" {
		return core.ShortNative(nativeID)
	}
	return name
}

func adoptFooter(name, nativeID string) string {
	short := core.ShortNative(nativeID)
	name = adoptDisplayName(name, nativeID)
	if name == short {
		return short
	}
	return name + " · " + short
}

func adoptCardTitle(name, title, nativeID string) string {
	if strings.TrimSpace(title) == "" {
		title = "本地会话"
	}
	return cut(adoptDisplayName(name, nativeID)+" · "+title, core.AdoptTitleRunes)
}

// topicAppLink opens the adopted DM topic. Feishu returns this shape from its
// message lookup; the gateway builds it from the confirmed chat and thread IDs.
func topicAppLink(chatID, threadID string) string {
	return core.TopicAppLink(chatID, threadID)
}

// chosenAdoptName prefers the name reported by the current request, then the
// name recorded with the topic, and finally the short native ID.
func chosenAdoptName(requestName, storedName, nativeID string) string {
	if strings.TrimSpace(requestName) != "" {
		return adoptDisplayName(requestName, nativeID)
	}
	if strings.TrimSpace(storedName) != "" {
		return adoptDisplayName(storedName, nativeID)
	}
	return adoptDisplayName("", nativeID)
}

// syncAdoptionName records a newly observed local session name and refreshes
// the root card title when it changed. The name is stored even if the card
// update fails so later replies and /status stay current; the stale card is
// reported as a warning.
func (g *Gateway) syncAdoptionName(ctx context.Context, record *store.AdoptedTopic, requestName string) []string {
	if strings.TrimSpace(requestName) == "" {
		return nil
	}
	name := adoptDisplayName(requestName, record.Native)
	if name == record.SessionName {
		return nil
	}
	warnings := []string{}
	if record.Root != "" {
		if err := g.transport.Update(ctx, record.Root, attachCard(name, record.Title, record.Work, record.Native)); err != nil {
			g.audit(record.Room, "adopt_card_update_failed", core.Args{"native": record.Native, "error": core.Redact(err.Error(), g.secrets)})
			warnings = append(warnings, "根卡片标题更新失败；会话名已记录")
		} else {
			g.audit(record.Room, "adopt_card_updated", core.Args{"native": record.Native})
		}
	}
	if err := g.store.SetAdoptionSessionName(record.Native, name); err != nil {
		warnings = append(warnings, "会话名写入失败")
		return warnings
	}
	record.SessionName = name
	return warnings
}

func attachCard(name, title, work, nativeID string) core.Args {
	body := "工作目录：" + work + "\n会话 ID：`" + nativeID + "`\n恢复终端：`claude --resume " + nativeID + "`"
	return core.Args{
		"config": core.Args{"wide_screen_mode": true},
		"header": core.Args{"title": core.Args{"tag": "plain_text", "content": adoptCardTitle(name, title, nativeID)}},
		"elements": []any{
			core.Args{"tag": "div", "text": core.Args{"tag": "lark_md", "content": body}},
		},
	}
}

func attachSummary(summary string) string {
	if strings.TrimSpace(summary) == "" {
		return "已接管本地 Claude Code 会话。终端退出后，直接在话题内发消息即可继续。"
	}
	return "已接管本地 Claude Code 会话。终端退出后，直接在话题内发消息即可继续。\n\n交接摘要：\n" + summary
}

func cleanAttachText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, value)
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > limit {
		value = cut(value, limit)
	}
	return value
}

func adoptedChatID(room string) string {
	chat, _, _, ok := core.ParseRoomKey(room)
	if !ok {
		return ""
	}
	return chat
}

func existingAdoptionResult(record store.AdoptedTopic, holderPID int, holderStarted int64) (core.AttachResult, error) {
	result := core.AttachResult{State: record.State, Room: record.Room, RootMessageID: record.Root, ThreadID: record.Thread, ChatID: adoptedChatID(record.Room), SessionName: record.SessionName, TopicURL: topicAppLink(adoptedChatID(record.Room), record.Thread)}
	switch record.State {
	case store.AdoptedOpen:
		result.OK = true
		return result, nil
	case store.AdoptedPending:
		if time.Since(time.UnixMilli(record.Created)) > adoptedPendingTTL {
			record.State = store.AdoptedUncertain
			result.State = record.State
			result.Error = "本地会话认领停留在未完成状态；请检查飞书是否出现话题与根消息 " + record.Root + "，不会自动重发。"
			return result, nil
		}
		result.Error = "本地会话认领正在处理中；请稍后在飞书查看，不要重复发送。"
		return result, nil
	case store.AdoptedUncertain:
		result.Error = "本地会话认领结果未确认；请检查飞书是否出现话题与根消息 " + record.Root + "，不会自动重发。"
		return result, nil
	default:
		return result, errors.New("本地会话认领状态无效")
	}
}

func (g *Gateway) existingAdoption(ctx context.Context, req core.AttachRequest, record store.AdoptedTopic) (core.AttachResult, error) {
	if record.User != req.User {
		return core.AttachResult{}, errors.New("该原生会话属于其他私聊，不能在此认领")
	}
	if record.State == store.AdoptedOpen {
		chat, dm, _, ok := core.ParseRoomKey(record.Room)
		if !ok || dm != req.User || chat == "" {
			return core.AttachResult{}, errors.New("该原生会话已绑定到其他私聊话题")
		}
		work, ok := g.adoptedWork(req.User, record.Work)
		if !ok {
			return core.AttachResult{}, errors.New("认领记录的工作目录已不可用或不在 adoptRoots 内")
		}
		if req.CWD != "" {
			expected, err := workspace.PhysicalDirectory(req.CWD)
			if err != nil || !core.SamePath(expected, work) {
				return core.AttachResult{}, errors.New("本地会话工作目录与认领记录不一致")
			}
		}
		if record.HolderPID > 0 && g.holderAlive(record) &&
			(record.HolderPID != req.HolderPID || record.HolderStarted != req.HolderStarted) {
			g.audit(record.Room, "adopt_reattach_rejected", core.Args{"native": record.Native, "user": req.User, "reason": "holder_active", "pid": record.HolderPID})
			return core.AttachResult{}, fmt.Errorf("该会话仍由本地终端 PID %d 持有；请先退出该终端或 /stop 后再认领", record.HolderPID)
		}
		current, found, err := g.store.CurrentSession(record.Room)
		if err != nil {
			return core.AttachResult{}, err
		}
		if !found || current.ID != record.Session {
			return core.AttachResult{}, errors.New("该话题已切换到其他会话；请在话题内用 /resume 选择原会话后再认领")
		}
		// Re-attaching a local terminal is a second writer. Refuse while the
		// gateway room still has a live worker, queued input, unfinished output,
		// action or approval; the user must /stop first.
		inputUnlock, err := g.inputLocks.lockContext(ctx, record.Room)
		if err != nil {
			return core.AttachResult{}, err
		}
		defer inputUnlock()
		dispatchUnlock, err := g.dispatchLocks.lockContext(ctx, record.Room)
		if err != nil {
			return core.AttachResult{}, err
		}
		defer dispatchUnlock()
		if reason, busyErr := g.adoptedRoomBusy(record); busyErr != nil {
			return core.AttachResult{}, busyErr
		} else if reason != "" {
			g.audit(record.Room, "adopt_reattach_rejected", core.Args{"native": record.Native, "user": req.User, "reason": reason})
			return core.AttachResult{}, errors.New("该话题仍有运行中的任务、待处理输入、未完成输出或审批。请先在话题里 /stop，等运行结束后再认领")
		}
		if err = g.store.UpdateAdoptionHolder(record.Native, req.HolderPID, req.HolderStarted); err != nil {
			return core.AttachResult{}, err
		}
		record.HolderPID, record.HolderStarted = req.HolderPID, req.HolderStarted
		g.setAdoptEndpoint(record.Room, adoptEndpoint{Socket: req.MessagingSocket, Token: req.MessagingToken, ConfigPath: req.ConfigPath, Native: record.Native})
		result, err := existingAdoptionResult(record, req.HolderPID, req.HolderStarted)
		if err == nil {
			result.Warnings = append(result.Warnings, g.syncAdoptionName(ctx, &record, req.SessionName)...)
			result.SessionName = record.SessionName
			g.audit(record.Room, "adopt_reattached", core.Args{"native": record.Native, "user": req.User, "pid": req.HolderPID})
		}
		return result, err
	}
	// Pending or uncertain: report only. A retry must never resend the card.
	if record.State == store.AdoptedPending && time.Since(time.UnixMilli(record.Created)) > adoptedPendingTTL {
		if err := g.store.MarkAdoptionUncertain(record.Native); err != nil {
			return core.AttachResult{}, err
		}
		record.State = store.AdoptedUncertain
	}
	result, err := existingAdoptionResult(record, req.HolderPID, req.HolderStarted)
	if err != nil {
		return result, err
	}
	g.audit(record.Room, "adopt_status_reported", core.Args{"native": record.Native, "user": req.User, "state": record.State})
	return result, nil
}

func (g *Gateway) attach(ctx context.Context, req core.AttachRequest) (core.AttachResult, error) {
	if ctx.Err() != nil {
		return core.AttachResult{}, ctx.Err()
	}
	if req.ID == "" || len(req.ID) > 64 {
		return core.AttachResult{}, errors.New("认领请求缺少有效 ID")
	}
	if req.At > 0 && time.Since(time.UnixMilli(req.At)) > adoptRequestTTL {
		return core.AttachResult{}, errors.New("认领请求已过期，请重新发起")
	}
	if !core.ValidThreadRoot(req.Native) {
		return core.AttachResult{}, errors.New("本地会话 ID 无效")
	}
	if req.HolderPID < 0 || req.HolderStarted < 0 {
		return core.AttachResult{}, errors.New("本地终端进程信息无效")
	}
	if _, ok := g.config.DM(req.User); !ok || !g.config.AdoptEnabled(req.User) {
		return core.AttachResult{}, errors.New("该私聊未启用本地会话认领")
	}
	dm, _ := g.config.DM(req.User)
	project, ok := g.config.Project(dm.ProjectID)
	if !ok || project.NativeConfig != "inherit" {
		return core.AttachResult{}, errors.New("认领要求项目的 nativeConfig 为 inherit")
	}
	title := strings.Join(strings.Fields(cleanAttachText(req.Title, core.AdoptTitleRunes)), " ")
	if title == "" {
		title = "本地会话认领"
	}
	name := adoptDisplayName(req.SessionName, req.Native)
	summary := cleanAttachText(req.Summary, core.AdoptSummaryRunes)

	// Serialize one native ID at a time. The unique key remains the durable
	// guard; this avoids two concurrent requests both observing "not found".
	unlock, err := g.adoptLocks.lockContext(ctx, req.Native)
	if err != nil {
		return core.AttachResult{}, err
	}
	defer unlock()

	record, found, err := g.store.AdoptedTopicByNative(req.Native)
	if err != nil {
		return core.AttachResult{}, err
	}
	if found {
		return g.existingAdoption(ctx, req, record)
	}
	_, configDir, override, err := workspace.InheritedConfig()
	if err != nil {
		return core.AttachResult{}, err
	}
	transcript, err := native.LocateTranscript(configDir, req.Native)
	if err != nil {
		return core.AttachResult{}, err
	}
	work, err := workspace.PhysicalDirectory(transcript.CWD)
	if err != nil {
		return core.AttachResult{}, errors.New("本地会话工作目录不可用")
	}
	if !native.SameProjectDir(native.EncodeProjectDir(work), filepath.Base(filepath.Dir(transcript.Path))) {
		return core.AttachResult{}, errors.New("本地会话的物理工作目录与 transcript 项目目录不一致（符号链接或大小写差异）；请从该目录的原始物理路径重启 Claude Code 后再认领")
	}
	if req.CWD != "" {
		expected, e := workspace.PhysicalDirectory(req.CWD)
		if e != nil || !core.SamePath(expected, work) {
			return core.AttachResult{}, errors.New("本地会话工作目录与网关记录不一致")
		}
	}
	if !g.config.AdoptWorkAllowed(req.User, work) {
		return core.AttachResult{}, errors.New("本地会话工作目录不在 adoptRoots 内")
	}
	warnings := []string{}
	if warning := adoptedModelWarning(g.gatewayModel(project), transcript.Model); warning != "" {
		warnings = append(warnings, warning)
	}

	record, created, err := g.store.BeginAdoption(req.Native, req.User, work, req.HolderPID, req.HolderStarted)
	if err != nil {
		return core.AttachResult{}, err
	}
	if !created {
		return g.existingAdoption(ctx, req, record)
	}
	if err = g.store.SetAdoptionIdentity(req.Native, name, title); err != nil {
		_ = g.store.DeleteUnsentAdoption(req.Native)
		return core.AttachResult{}, err
	}
	sender, ok := g.transport.(core.UserCardSender)
	if !ok {
		_ = g.store.DeleteUnsentAdoption(req.Native)
		return core.AttachResult{}, errors.New("当前传输不支持认领卡片")
	}
	rootID, chatID, err := sender.SendUserCard(core.WithDeliveryID(ctx, "attach:"+req.ID+":root"), req.User, attachCard(name, title, work, req.Native))
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
			return core.AttachResult{State: store.AdoptedUncertain, RootMessageID: rootID, ChatID: chatID, SessionName: name, Error: "根卡片发送结果未确认；请检查私聊中的根消息，不会自动重发。"}, nil
		}
		_ = g.store.DeleteUnsentAdoption(req.Native)
		return core.AttachResult{}, err
	}
	room, ok := g.config.AdoptedTopicRoom(chatID, req.User, rootID)
	if !ok {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.AttachResult{State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: "根卡片已发送，但话题房间配置校验失败；请核查根消息 " + rootID + "。"}, nil
	}
	// Persist the known root and room before the session transaction. If that
	// transaction fails, the topic can still match replies and report its
	// uncertain state instead of falling back to the ordinary DM session.
	_ = g.store.SetAdoptionRootEvidence(req.Native, room.Key, rootID)
	sessionID := core.NewID()
	now := time.Now().UnixMilli()
	if err = g.store.BindAdoptionRoot(req.Native, room.Key, rootID, sessionID, configDir, override, now); err != nil {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.AttachResult{State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: "根卡片已发送，但会话记录写入失败；请核查根消息 " + rootID + "。"}, nil
	}
	replyID, err := g.transport.Send(core.WithDeliveryID(ctx, "attach:"+req.ID+":thread"), room, attachSummary(summary), rootID, "")
	if err != nil {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.AttachResult{State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: "根卡片已发送，但话题回复失败；请核查根消息 " + rootID + "。"}, nil
	}
	message, err := g.transport.Fetch(ctx, replyID, room)
	if err != nil || message == nil || message.ThreadID == "" || message.RootID != rootID {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.AttachResult{State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, SessionName: name, Error: "根卡片已发送，但话题 ID 未能确认；请核查根消息 " + rootID + "。"}, nil
	}
	if err = g.store.OpenAdoption(req.Native, message.ThreadID); err != nil {
		_ = g.store.MarkAdoptionUncertain(req.Native)
		return core.AttachResult{State: store.AdoptedUncertain, ChatID: chatID, RootMessageID: rootID, ThreadID: message.ThreadID, SessionName: name, TopicURL: topicAppLink(chatID, message.ThreadID), Error: "根卡片已发送且话题已建立，但状态写入失败；请核查根消息 " + rootID + "。"}, nil
	}
	g.audit(room.Key, "adopt", core.Args{
		"native": req.Native, "user": req.User, "work": work,
		"root": rootID, "thread": message.ThreadID, "holderPid": req.HolderPID,
	})
	g.setAdoptEndpoint(room.Key, adoptEndpoint{Socket: req.MessagingSocket, Token: req.MessagingToken, ConfigPath: req.ConfigPath, Native: req.Native})
	return core.AttachResult{
		OK:            true,
		State:         store.AdoptedOpen,
		Room:          room.Key,
		ChatID:        chatID,
		RootMessageID: rootID,
		ThreadID:      message.ThreadID,
		SessionName:   name,
		TopicURL:      topicAppLink(chatID, message.ThreadID),
		Warnings:      warnings,
	}, nil
}
