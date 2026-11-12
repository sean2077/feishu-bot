package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
	"time"
	"unicode"
)

var phases = map[string]string{"not_started": "尚未启动", "suspended": "已暂停运行，保留会话", "starting": "正在启动原生会话", "stopping": "正在停止，等待退出确认", "termination_unconfirmed": "停止未确认，需要维护者检查", "compacting": "正在整理原生上下文", "awaiting_answer": "等待审批或回答", "working": "处理中", "delivery_pending": "有输出尚未完整送达", "queued": "输入已保存，等待运行", "background": "原生后台工作或监控中", "connected_unknown": "已连接，后台状态尚未确认", "idle": "空闲，可继续对话"}

func jsonPretty(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
func (g *Gateway) statusView(r core.Room) (core.Args, error) {
	sessions, e := g.store.ListSessions(r.Key)
	if e != nil {
		return nil, e
	}
	var saved *core.Session
	for _, s := range sessions {
		if s.State == "open" {
			v := s
			saved = &v
			break
		}
	}
	inputs := core.Args{"queued": int64(0), "sent": int64(0), "consumed": int64(0), "uncertain": int64(0)}
	rows, e := g.store.DB.Query("SELECT status,count(*) FROM inbox WHERE room=? AND status IN ('queued','sent','consumed','uncertain') GROUP BY status", r.Key)
	if e != nil {
		return nil, e
	}
	total := int64(0)
	for rows.Next() {
		var status string
		var n int64
		if rows.Scan(&status, &n) == nil {
			inputs[status] = n
			total += n
		}
	}
	rows.Close()
	var outputs int64
	if e = g.store.DB.QueryRow("SELECT count(*) FROM outbox WHERE room=? AND state NOT IN ('done','cancelled') AND kind!='mention'", r.Key).Scan(&outputs); e != nil {
		return nil, e
	}
	outboxStates := core.Args{}
	rows, e = g.store.DB.Query("SELECT state,count(*) FROM outbox WHERE room=? GROUP BY state", r.Key)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var state string
		var count int64
		if e = rows.Scan(&state, &count); e != nil {
			rows.Close()
			return nil, e
		}
		outboxStates[state] = count
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	var snapshotCount, snapshotBytes int64
	if e = g.store.DB.QueryRow("SELECT count(*),coalesce(sum(length(CAST(snapshot AS BLOB))),0) FROM feishu_cards WHERE room=?", r.Key).Scan(&snapshotCount, &snapshotBytes); e != nil {
		return nil, e
	}
	state := "not_started"
	if saved != nil {
		state = "suspended"
	}
	phase := state
	background := any(int64(0))
	pinned := false
	approvals := 0
	effectivePermissionMode := ""
	s := g.getSlot(r.Key)
	if s != nil {
		approvals = g.approvalCount(s.session.ID)
		s.mu.Lock()
		ss := s.session
		saved = &ss
		pinned = s.pinned
		effectivePermissionMode = s.effectivePermissionMode
		background = any(s.background)
		if s.background < 0 {
			background = nil
		}
		state = "connected"
		if s.busy {
			state = "working"
		}
		if s.stopUnconfirmed {
			state = "termination_unconfirmed"
			phase = state
		} else if !s.live {
			phase = "stopping"
		} else if s.worker == nil {
			phase = "starting"
		} else if s.compacting != "" {
			phase = "compacting"
		} else if approvals > 0 {
			phase = "awaiting_answer"
		} else if s.busy {
			phase = "working"
		} else if outputs > 0 {
			phase = "delivery_pending"
		} else if inputs["queued"].(int64) > 0 {
			phase = "queued"
		} else if s.pinned || s.background > 0 {
			phase = "background"
		} else if s.background < 0 {
			phase = "connected_unknown"
		} else {
			phase = "idle"
		}
		s.mu.Unlock()
	} else if outputs > 0 {
		phase = "delivery_pending"
	} else if inputs["queued"].(int64) > 0 {
		phase = "queued"
	}
	view := core.Args{"project": r.ProjectID, "state": state, "phase": phase, "background": background, "monitorPinned": pinned, "pendingOrUnconfirmed": total, "inputs": inputs, "approvals": approvals, "pendingOutputs": outputs}
	project, _ := g.config.Project(r.ProjectID)
	profile, profileSource := "default", "default"
	if project.ModelProfile != "" {
		profile, profileSource = project.ModelProfile, "project"
	}
	requestedPermissionMode := project.PermissionMode
	if requestedPermissionMode == "" {
		requestedPermissionMode = "acceptEdits"
	}
	view["permissionMode"] = core.Args{"requested": requestedPermissionMode, "effective": effectivePermissionMode}
	if r.Thread != "" {
		view["threadRoot"] = r.Thread
		view["workspaceBinding"] = r.WorkspaceCWD
	}
	view["outboxStates"] = outboxStates
	view["cardSnapshots"] = core.Args{"count": snapshotCount, "bytes": snapshotBytes}
	maintenanceCtx, cancelMaintenance := context.WithTimeout(context.Background(), time.Second)
	if at, ok, err := g.store.LastFullMaintenance(maintenanceCtx); err == nil {
		if ok {
			view["maintenance"] = core.Args{"lastFullAt": time.UnixMilli(at).UTC().Format(time.RFC3339)}
		} else {
			view["maintenance"] = core.Args{"lastFullAt": nil}
		}
	}
	cancelMaintenance()
	view["profile"] = profile
	view["profileSource"] = profileSource
	if saved != nil {
		view["session"] = saved.ID
		p, err := g.store.Preferences(r.Key, saved.ID)
		if err != nil {
			return nil, err
		}
		selection := profileSelection(g.config, project, p.Profile)
		view["profile"] = selection.Profile
		view["profileSource"] = selection.Source
		view["sessionName"] = p.Name
		if p.Name == "" {
			view["sessionName"] = p.NativeTitle
		}
		if stringArg(view, "sessionName") == "" {
			view["sessionName"] = cut(saved.ID, 8)
		}
		view["manualName"] = p.Name
		view["nativeTitle"] = p.NativeTitle
		title, err := g.store.TitleSync(r.Key, saved.ID)
		if err != nil {
			return nil, err
		}
		view["titleSync"] = core.Args{"desired": title.Desired, "revision": title.Revision, "confirmed": title.Confirmed, "state": title.State, "updatedAt": title.Updated}
		if p.RequestedProfile != nil {
			view["requestedProfile"] = profileLabel(*p.RequestedProfile)
		}
		if saved.CCID != "" {
			view["nativeSessionId"] = saved.CCID
		}
	}
	return view, nil
}
func (g *Gateway) management(cmd, arg string, r core.Room, actor string) (string, bool, error) {
	if !contains([]string{"/help", "/status", "/sessions", "/outputs", "/usage", "/copy"}, cmd) {
		return "", false, nil
	}
	if cmd != "/sessions" && arg != "" && (arg != "--json" || cmd == "/help") {
		usage := "用法：" + cmd
		if cmd != "/help" {
			usage += " [--json]"
		}
		return usage, true, nil
	}
	manage := canManage(r, actor)
	if cmd == "/help" {
		text := "**日常对话**\n群里请明确 @ 机器人；私聊直接发送。忙时普通消息仍交给原生会话处理。\n/btw 正文、/ps 正文：仅补充当前前台任务，不新建任务，不代替审批回答；接收确认不代表模型已读取。\n\n**查看与回答**\n/status 当前工作、待回答事项及交付情况\n/sessions [关键词] 搜索本聊天历史会话并翻页\n/copy 把最近一条完整回复作为纯文本发到当前聊天，便于复制\n/inputs 每条待处理输入、等待原因和可申请取消范围\n/outputs 尚未完整送达的回复和文件\n/usage 当前选中会话的 SDK 用量，不估造费用或上下文占用\n/answer 请求ID 回答（多题也可使用卡片）\n\n**会话管理**\n/stop 停止运行，保留上下文；下条消息继续\n/compact [说明] 在轮次边界执行原生压缩\n/new 明确新建会话\n/resume 会话ID 选择本聊天历史\n/cancel-input 输入ID 取消经核实尚未交给原生进程的输入\n/discard-output 输出ID 放弃同轮依赖交付，原生任务继续、不标 Done\n/retry-output 输出ID 明确补发；结果未知时可能重复\n"
		if manage {
			text += "你可以管理本聊天会话。工具审批仍限项目维护者。"
		} else {
			text += "新建、恢复、停止、压缩和补发限项目维护者；私聊本人可管理自己的会话。"
		}
		text += "\n/model [配置名] 查看或选择管理员定义的模型 / effort 配置（/profile 同义）\n/rename 名称 命名当前会话；/rename - 清除\n/resume 名称或唯一短ID 恢复本聊天的会话\n/artifacts [文件ID] 查看或重新取得已完成的文件，不启动模型\n"
		return text + "\n\n排障需要机器数据时，可用 /status --json、/sessions --json、/outputs --json、/inputs --json、/usage --json、/model --json、/artifacts --json。", true, nil
	}
	if cmd == "/copy" {
		text, ok := g.latestReplyText(r)
		if !ok {
			return "没有可复制的完整回复。", true, nil
		}
		g.sendCopyText(r, text)
		return "已发送可复制的纯文本。", true, nil
	}
	if cmd == "/usage" {
		text, e := g.usageReply(r, arg)
		return text, true, e
	}
	if cmd == "/status" {
		v, e := g.statusView(r)
		if e != nil {
			return "", true, e
		}
		if arg == "--json" {
			ctx, cancel := context.WithTimeout(g.ctx, 2*time.Second)
			stats, err := g.store.TableStats(ctx, r.Key)
			cancel()
			v["storage"] = stats
			if err != nil {
				v["storageError"] = "存储诊断未完成，请查看服务日志"
				g.audit(r.Key, "storage_diagnostics_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
			}
			if manage {
				if d, ok := g.transport.(interface{ Diagnostics() core.Args }); ok {
					v["transport"] = d.Diagnostics()
				}
			}
			return jsonPretty(v), true, nil
		}
		phase := stringArg(v, "phase")
		inputs := argsOf(v["inputs"])
		session := stringArg(v, "session")
		if session == "" {
			session = "尚无会话"
		}
		native := stringArg(v, "nativeSessionId")
		if native == "" {
			native = "尚未建立"
		}
		background := fmt.Sprint(v["background"])
		if v["background"] == nil {
			background = "未知（不会据此回收进程）"
		}
		pin := "否"
		if boolArg(v, "monitorPinned") {
			pin = "是"
		}
		text := fmt.Sprintf("**本聊天状态**\n状态：%s\n项目：%s\n当前会话：`%s`\n原生会话：`%s`\n\n输入 · 排队：%v · 已交给原生：%v · 已消费待结果：%v · 结果未知：%v\n等待审批或回答：%v · 待交付输出：%v\n后台活动：%s · 监控保留：%s", phases[phase], r.ProjectID, session, native, inputs["queued"], inputs["sent"], inputs["consumed"], inputs["uncertain"], v["approvals"], v["pendingOutputs"], background, pin)
		if mode := argsOf(v["permissionMode"]); mode != nil {
			line := "\n权限模式：" + stringArg(mode, "requested")
			if effective := stringArg(mode, "effective"); effective != "" {
				line += "（原生确认：" + effective + "）"
			} else {
				line += "（原生尚未确认）"
			}
			text += line
		}
		if r.Thread != "" {
			binding := "使用群默认目录"
			if r.WorkspaceCWD != "" {
				binding = r.WorkspaceCWD
			}
			text += fmt.Sprintf("\n话题根消息：`%s`\n话题工作区绑定：%s", r.Thread, binding)
		}
		if name := stringArg(v, "sessionName"); name != "" {
			text += "\n会话名称：" + name
		}
		if title := argsOf(v["titleSync"]); stringArg(title, "desired") != "" {
			state := stringArg(title, "state")
			labels := map[string]string{"pending": "等待同步", "confirmed": "已确认", "not_sent": "未发出", "rejected": "原生端拒绝", "unconfirmed": "结果未确认"}
			if label := labels[state]; label != "" {
				text += "\n原生标题同步：" + label
			}
		}
		if profile := stringArg(v, "profile"); profile != "" {
			text += "\n模型配置：" + profile + "（" + profileSourceLabel(stringArg(v, "profileSource")) + "）"
		}
		if profile := stringArg(v, "requestedProfile"); profile != "" {
			text += "\n待切换配置：" + profile + "（等待安全轮次边界）"
		}
		if phase == "termination_unconfirmed" {
			text += "\n\n不会启动替代进程或切换会话；请维护者核实服务诊断与进程状态。"
		}
		if phase == "suspended" {
			text += "\n\n下条消息继续原会话，无需 /new。"
		}
		if phase == "not_started" {
			text += "\n\n发送一条消息即可开始；群聊请明确 @ 机器人。"
		}
		if n, _ := numberArg(inputs, "uncertain"); n > 0 {
			text += "\n结果未知的输入不会自动重放，请先核实外部结果，勿直接重复执行。"
		}
		if n, _ := numberArg(v, "pendingOutputs"); n > 0 {
			text += "\n用 /outputs 查看尚未送达的回复或文件，不必让模型重新执行任务。"
		}
		if maintenance := argsOf(v["maintenance"]); maintenance != nil {
			if at := stringArg(maintenance, "lastFullAt"); at != "" {
				text += "\n维护：最近完整清理 " + at
			} else {
				text += "\n维护：尚未完成一次完整清理"
			}
		}
		return text, true, nil
	}
	if cmd == "/sessions" {
		page, err := g.sessionPage(r, arg)
		if err != nil {
			return err.Error(), true, nil
		}
		return sessionPageText(page, manage), true, nil
	}

	rows, e := g.store.DB.Query(`SELECT o.id,o.state,o.file_state,o.delivery_error,o.kind,o.retry_count,o.retry_after,
		EXISTS(SELECT 1 FROM outbox f WHERE f.room=o.room AND f.session=o.session AND f.kind='file' AND f.state NOT IN ('done','cancelled'))
		FROM outbox o WHERE o.room=? AND o.state NOT IN ('done','cancelled') AND o.kind!='mention' ORDER BY o.at DESC,o.id DESC LIMIT 20`, r.Key)
	if e != nil {
		return "", true, e
	}
	items := []core.Args{}
	for rows.Next() {
		var id, state, fileState, kind string
		var detail sql.NullString
		var retryCount int
		var retryAfter int64
		var pendingFiles bool
		if e = rows.Scan(&id, &state, &fileState, &detail, &kind, &retryCount, &retryAfter, &pendingFiles); e != nil {
			rows.Close()
			return "", true, e
		}
		stage := "reply"
		if kind == "file" || kind == "artifact" || fileState != "none" && fileState != "done" {
			stage = "file"
		}
		filePending := kind == "reply" && (pendingFiles || fileState != "none" && fileState != "done")
		reason := "pending"
		switch {
		case state == "uncertain" || fileState == "uncertain":
			reason = "uncertain"
		case strings.Contains(detail.String, "Automatic retry budget exhausted"):
			reason = "retry_budget"
		case state == "blocked":
			reason = "rejected"
		case strings.Contains(detail.String, "HTTP 429") || strings.Contains(detail.String, "code 99991400"):
			reason = "rate_limited"
		case filePending:
			reason = "file_pending"
		}
		item := core.Args{"id": id, "state": state, "stage": stage, "reason": reason, "retryCount": retryCount, "retryAfter": retryAfter, "nativeTurnEnded": kind == "reply", "filePending": filePending, "uncertain": state == "uncertain" || fileState == "uncertain"}
		if detail.Valid && detail.String != "" {
			item["detail"] = "交付尚未完成，请查看服务诊断。"
			if manage {
				item["detail"] = cut(strings.Map(func(r rune) rune {
					if unicode.IsControl(r) {
						return ' '
					}
					return r
				}, core.Redact(detail.String, g.secrets)), 600)
			}
		}
		items = append(items, item)
	}
	rows.Close()
	if arg == "--json" {
		return jsonPretty(items), true, nil
	}
	if len(items) == 0 {
		return "本聊天没有待交付输出。这里只表示交付记录已清空，不代表所有输入都已执行完成；输入状态见 /status。", true, nil
	}
	lines := []string{"**待交付输出**", "显示最近 20 条。"}
	for _, item := range items {
		stage := "回复"
		if item["stage"] == "file" {
			stage = "文件"
		}
		state := map[string]string{"pending": "等待交付", "uncertain": "结果未知", "blocked": "自动重试已暂停", "sending": "正在发送", "preparing": "正在准备"}[stringArg(item, "state")]
		lines = append(lines, "", stage+" · "+state+"\n`"+stringArg(item, "id")+"`")
		reasons := map[string]string{"rejected": "已确认拒绝；请先修复权限、参数或服务配置。", "retry_budget": "已达到自动重试预算；请处理原因后明确补发。", "rate_limited": "服务限流；自动重试遵守服务返回的等待时间。", "file_pending": "模型本轮已结束，完整文件仍待送达；尚未标记 Done。"}
		if reason := reasons[stringArg(item, "reason")]; reason != "" {
			lines = append(lines, reason)
		}
		if boolArg(item, "filePending") && stringArg(item, "reason") != "file_pending" {
			lines = append(lines, "模型本轮已结束，文件仍待交付；尚未标记 Done。")
		}
		if manage {
			lines = append(lines, fmt.Sprintf("交付失败次数：%v", item["retryCount"]))
			if at, ok := item["retryAfter"].(int64); ok && at > 0 && stringArg(item, "state") == "pending" {
				lines = append(lines, "最早自动重试："+timeLabel(at))
			}
			if detail := stringArg(item, "detail"); detail != "" {
				lines = append(lines, "诊断："+detail)
			}
		}
		if boolArg(item, "uncertain") {
			lines = append(lines, "先核实是否已经收到；明确补发可能重复，不会自动重放未知写入。")
		}
		if stringArg(item, "state") == "blocked" {
			lines = append(lines, "交付遇到明确失败或已达到自动重试上限；请先检查服务诊断并处理原因，再明确补发。")
		}
		if manage && contains([]string{"pending", "uncertain", "blocked"}, stringArg(item, "state")) {
			lines = append(lines, "明确补发：`/retry-output "+stringArg(item, "id")+"`", "放弃同轮依赖交付（不停止模型、不标 Done）：`/discard-output "+stringArg(item, "id")+"`")
		}
	}
	lines = append(lines, "", "补发只处理已保存的输出，不重新调用模型或执行原任务。")
	return strings.Join(lines, "\n"), true, nil
}
func (g *Gateway) command(ctx context.Context, r core.Room, m core.Message) error {
	cmd, arg := splitCommand(m.Text)
	if handled, err := g.operationCommand(ctx, r, m); handled {
		return err
	}
	if cmd == "/sessions" {
		return g.sessionsReply(ctx, r, m)
	}
	if cmd == "/model" || cmd == "/profile" {
		return g.modelCommand(r, m, arg)
	}
	if cmd == "/rename" {
		return g.renameCommand(r, m, arg)
	}
	if cmd == "/artifacts" {
		return g.artifactCommand(ctx, r, m, arg)
	}
	if view, ok, e := g.management(cmd, arg, r, m.SenderID); ok {
		if e != nil {
			return e
		}
		return g.notice(g.ctx, r, view, &m)
	}
	if cmd == "/retry-output" {
		if !canManage(r, m.SenderID) {
			g.notify(r, "需要维护者批准补发。", &m)
			return nil
		}
		ok, e := g.deliverOutput(g.ctx, arg, r, true)
		text := "未补发：记录不存在、已发送或渠道仍不可用。"
		if ok {
			text = "已补发。此前结果不明确时可能出现重复消息。"
		}
		g.notify(r, text, &m)
		return e
	}
	if cmd == "/answer" {
		id, text := splitCommand(arg)
		g.notify(r, g.answer(r, m.SenderID, id, text), &m)
		return nil
	}
	if !canManage(r, m.SenderID) {
		g.notify(r, "此操作需要项目维护者。", &m)
		return nil
	}
	if cmd == "/resume" {
		id, err := g.store.ResolveSession(r.Key, arg)
		if err != nil {
			g.notify(r, "未切换会话："+err.Error()+"。用 /sessions 查看本聊天会话。", &m)
			return nil
		}
		arg = id
	}
	if e := g.stopRoom(ctx, r); e != nil {
		text := "原生进程停止失败，尚未确认退出。当前会话保留，不能启动替代进程或切换会话；请先检查服务诊断并核实进程。"
		if errors.Is(e, errStopState) {
			text = stopStateNotice
		}
		g.notify(r, text, &m)
		return e
	}
	switch cmd {
	case "/resume":
		if _, e := g.store.Resume(r.Key, arg); e != nil {
			return e
		}
		g.clearProfileWait(r.Key)
		g.notify(r, "已选择历史会话，旧运行未交付的补充已取消。下一条消息会恢复上下文；旧后台进程不会自动重启。", &m)
	case "/new":
		if _, e := g.store.DB.Exec("DELETE FROM inbox WHERE room=? AND status='background'", r.Key); e != nil {
			return e
		}
		if _, e := g.store.NewSession(r.Key); e != nil {
			return e
		}
		g.clearProfileWait(r.Key)
		g.notify(r, "已新建本聊天会话，旧运行未交付的补充已取消。", &m)
	case "/stop":
		g.notify(r, "已停止本聊天运行、后台进程和待交付输入；未交付的补充已取消。上下文和会话 ID 保留，下条消息继续原会话；只有 /new 新建会话。", &m)
	}
	return nil
}
