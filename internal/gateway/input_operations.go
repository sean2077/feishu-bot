package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

// operationCommand is dispatched asynchronously after Message releases its
// input lock. Cancellation re-enters that lock before excluding dispatch.
func (g *Gateway) operationCommand(ctx context.Context, r core.Room, m core.Message) (bool, error) {
	cmd, arg := splitCommand(m.Text)
	switch cmd {
	case "/inputs":
		if arg != "" && arg != "--json" {
			return true, g.notice(ctx, r, "用法：/inputs [--json]", &m)
		}
		text, err := g.inputsView(ctx, r, m.SenderID, arg == "--json")
		if err != nil {
			return true, err
		}
		return true, g.notice(ctx, r, text, &m)
	case "/cancel-input":
		if len(strings.Fields(arg)) != 1 {
			return true, g.notice(ctx, r, "用法：/cancel-input 输入ID。用 /inputs 查看完整 ID；图片批次会整体取消。", &m)
		}
		cancelled, err := g.cancelInput(ctx, r, arg, m.SenderID)
		if err != nil {
			text := "取消未完成，请查看服务诊断。"
			switch {
			case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
				text = "暂不能确认输入尚未交给原生进程：派发交接仍在进行。未宣称撤回；请稍后用 /inputs 核实。"
			case errors.Is(err, store.ErrInputNotFound):
				text = "本聊天未找到该输入。请复制 /inputs 中的完整 ID。"
			case errors.Is(err, store.ErrInputNotOwned):
				text = "只能取消自己尚未提交的输入；维护者可管理本聊天其他成员的输入。"
			case errors.Is(err, store.ErrInputHandedOff):
				text = "未取消：输入已离开可证明未提交的状态，或已取消。原生已接收、正在处理和结果未知的输入无法逐条撤回。"
			case errors.Is(err, store.ErrInputInvalid):
				text = "未取消：输入作者或图片批次绑定无法核实，请维护者检查服务诊断。"
			default:
				g.audit(r.Key, "input_cancel_failed", core.Args{"id": arg, "error": core.Redact(err.Error(), g.secrets)})
			}
			return true, g.notice(ctx, r, text, &m)
		}
		text := "已取消 1 条经派发锁确认尚未提交的输入；当前原生任务继续运行。"
		if len(cancelled) > 1 {
			text = fmt.Sprintf("已整体取消同一图片批次的 %d 条未提交输入；当前原生任务继续运行。", len(cancelled))
		}
		return true, g.notice(ctx, r, text, &m)
	case "/discard-output":
		return true, g.discardOutputCommand(ctx, r, m, arg)
	}
	return false, nil
}

func (g *Gateway) cancelInput(ctx context.Context, r core.Room, id, actor string) ([]store.InputRecord, error) {
	current, ok := RoomFromKey(g.config, r.Key)
	if !ok || !AuthorizedAuthor(current, actor) {
		return nil, store.ErrInputNotOwned
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	inputUnlock, err := g.inputLocks.lockContext(ctx, r.Key)
	if err != nil {
		return nil, err
	}
	defer inputUnlock()
	dispatchUnlock, err := g.dispatchLocks.lockContext(ctx, r.Key)
	if err != nil {
		return nil, err
	}
	defer dispatchUnlock()
	cancelled, err := g.store.CancelQueuedInput(ctx, r.Key, id, actor, canManage(current, actor), time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	g.clearImageTimer(r.Key)
	for _, v := range cancelled {
		g.kickReceipt(v.MessageID)
	}
	g.audit(r.Key, "input_cancelled_by_user", core.Args{"actor": actor, "id": id, "count": len(cancelled), "proof": "dispatch_excluded_before_handoff"})
	g.kick(r)
	return cancelled, nil
}

func (g *Gateway) inputsView(ctx context.Context, r core.Room, actor string, asJSON bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, more, err := g.store.Inputs(ctx, r.Key, 50)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	profileBlocked := g.profileWait[r.Key] != ""
	waiting := g.waiting[r.Key]
	g.mu.Unlock()
	compacting, busy, starting := false, false, false
	if s := g.getSlot(r.Key); s != nil {
		s.mu.Lock()
		compacting = s.compacting != ""
		busy = s.busy || len(s.deliveries) > 0 || s.activeRequests > 0
		starting = s.worker == nil
		s.mu.Unlock()
	}
	now := time.Now().UnixMilli()
	items := []core.Args{}
	lines := []string{"**输入明细**", "显示最近 50 条待处理或未核实输入。queued 只是保存状态；取消时还会核实派发边界。"}
	for _, v := range rows {
		reason := "等待派发检查"
		switch v.Status {
		case "sent":
			reason = "已交入原生输入流程，不能逐条撤回"
		case "consumed":
			reason = "原生已读取，等待结果"
		case "uncertain":
			reason = "提交或执行结果未知，保留证据，不自动重放"
		case "queued":
			switch {
			case !v.VerifiedPayload:
				reason = "持久化输入绑定无法核实"
			case profileBlocked:
				reason = "模型配置不可用，等待明确处理"
			case compacting:
				reason = "等待原生上下文整理完成"
			case v.Command == "compact" && busy:
				reason = "压缩命令等待前一轮结束"
			case !v.BatchClosed && v.BatchDeadline > now:
				reason = "等待图片批次收集窗口"
			case waiting:
				reason = "等待原生进程名额"
			case starting:
				reason = "等待原生进程启动"
			}
		}
		canRequest := v.Status == "queued" && v.VerifiedPayload && (canManage(r, actor) || v.Sender == actor)
		item := core.Args{"id": v.ID, "messageId": v.MessageID, "session": v.Session, "sender": v.Sender, "state": v.Status, "createdAt": v.At, "ageMs": max(int64(0), now-v.At), "reason": reason, "cancelRequiresDispatchCheck": true, "canRequestCancel": canRequest}
		if v.BatchID != "" {
			item["batchId"] = v.BatchID
			item["cancelScope"] = "whole_image_batch"
		}
		if v.Command != "" {
			item["command"] = v.Command
		}
		if v.SupplementTo != "" {
			item["supplementTo"] = v.SupplementTo
		}
		items = append(items, item)
		line := fmt.Sprintf("\n`%s`\n%s · 作者 %s · %s\n%s", v.ID, v.Status, v.Sender, timeLabel(v.At), reason)
		if v.BatchID != "" {
			line += "\n图片批次：" + v.BatchID + "（取消将作用于整个批次）"
		}
		if canRequest {
			line += "\n申请取消：`/cancel-input " + v.ID + "`"
		}
		lines = append(lines, line)
	}
	if asJSON {
		return jsonPretty(core.Args{"items": items, "more": more, "limit": 50}), nil
	}
	if len(items) == 0 {
		return "本聊天没有待处理或未核实输入。", nil
	}
	if more {
		lines = append(lines, "\n还有较早的未核实输入；此列表已达到 50 条显示上限。")
	}
	return strings.Join(lines, "\n"), nil
}
