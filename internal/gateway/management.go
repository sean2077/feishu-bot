package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
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
	if e = g.store.DB.QueryRow("SELECT count(*) FROM outbox WHERE room=? AND state NOT IN ('done','cancelled')", r.Key).Scan(&outputs); e != nil {
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
	s := g.getSlot(r.Key)
	if s != nil {
		approvals = g.approvalCount(s.session.ID)
		s.mu.Lock()
		ss := s.session
		saved = &ss
		pinned = s.pinned
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
	if saved != nil {
		view["session"] = saved.ID
		if saved.CCID != "" {
			view["nativeSessionId"] = saved.CCID
		}
	}
	return view, nil
}
func (g *Gateway) management(cmd, arg string, r core.Room, actor string) (string, bool, error) {
	if !contains([]string{"/help", "/status", "/sessions", "/outputs", "/usage"}, cmd) {
		return "", false, nil
	}
	if arg != "" && (arg != "--json" || cmd == "/help") {
		usage := "用法：" + cmd
		if cmd != "/help" {
			usage += " [--json]"
		}
		return usage, true, nil
	}
	manage := canManage(r, actor)
	if cmd == "/help" {
		text := "**日常对话**\n群里请明确 @ 机器人；私聊直接发送。忙时普通消息仍交给原生会话处理。\n/btw 正文、/ps 正文：仅补充当前前台任务，不新建任务，不代替审批回答；接收确认不代表模型已读取。\n\n**查看与回答**\n/status 当前工作、待回答事项及交付情况\n/sessions 本聊天历史会话\n/outputs 尚未完整送达的回复和文件\n/usage 当前选中会话的 SDK 用量，不估造费用或上下文占用\n/answer 请求ID 回答（多题也可使用卡片）\n\n**会话管理**\n/stop 停止运行，保留上下文；下条消息继续\n/compact [说明] 在轮次边界执行原生压缩\n/new 明确新建会话\n/resume 会话ID 选择本聊天历史\n/retry-output 输出ID 明确补发；结果未知时可能重复\n"
		if manage {
			text += "你可以管理本聊天会话。工具审批仍限项目维护者。"
		} else {
			text += "新建、恢复、停止、压缩和补发限项目维护者；私聊本人可管理自己的会话。"
		}
		return text + "\n\n排障需要机器数据时，可用 /status --json、/sessions --json、/outputs --json、/usage --json。", true, nil
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
		return text, true, nil
	}
	if cmd == "/sessions" {
		list, e := g.store.ListSessions(r.Key)
		if e != nil {
			return "", true, e
		}
		var total int
		if e = g.store.DB.QueryRow("SELECT count(*) FROM sessions WHERE room=?", r.Key).Scan(&total); e != nil {
			return "", true, e
		}
		current := ""
		for _, s := range list {
			if s.State == "open" {
				current = s.ID
			}
		}
		items := []core.Args{}
		for _, s := range list {
			items = append(items, core.Args{"id": s.ID, "state": s.State, "createdAt": s.CreatedAt, "current": s.ID == current})
		}
		if arg == "--json" {
			v := core.Args{"total": total, "recentLimit": 20, "sessions": items}
			if current != "" {
				v["current"] = current
			}
			return jsonPretty(v), true, nil
		}
		if len(items) == 0 {
			return "尚无历史会话。发送消息后会自动建立；查看列表不会启动模型。", true, nil
		}
		lines := []string{"**本聊天会话**", fmt.Sprintf("共 %d 个；显示最近 20 个，并保留当前会话。", total), "时间为 UTC；会话 ID 可直接复制。"}
		for _, s := range list {
			label := "历史会话"
			if s.ID == current {
				label = "当前会话"
			}
			lines = append(lines, "", label+" · "+timeLabel(s.CreatedAt)+"\n`"+s.ID+"`")
		}
		if manage {
			lines = append(lines, "", "恢复：/resume 会话ID。仅下条消息恢复上下文，旧后台工作不会自动重启。")
		} else {
			lines = append(lines, "", "恢复历史会话请联系项目维护者。")
		}
		return strings.Join(lines, "\n"), true, nil
	}
	rows, e := g.store.DB.Query("SELECT id,state,file_state,delivery_error,kind FROM outbox WHERE room=? AND state NOT IN ('done','cancelled') ORDER BY at DESC,id DESC LIMIT 20", r.Key)
	if e != nil {
		return "", true, e
	}
	items := []core.Args{}
	for rows.Next() {
		var id, state, fileState, kind string
		var detail sql.NullString
		if e = rows.Scan(&id, &state, &fileState, &detail, &kind); e != nil {
			rows.Close()
			return "", true, e
		}
		stage := "reply"
		if kind == "file" || fileState != "none" && fileState != "done" {
			stage = "file"
		}
		item := core.Args{"id": id, "state": state, "stage": stage, "uncertain": state == "uncertain" || fileState == "uncertain"}
		if detail.Valid && detail.String != "" {
			item["detail"] = "交付尚未完成，请查看服务诊断。"
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
		state := map[string]string{"pending": "等待交付", "uncertain": "结果未知", "sending": "正在发送", "preparing": "正在准备"}[stringArg(item, "state")]
		lines = append(lines, "", stage+" · "+state+"\n`"+stringArg(item, "id")+"`")
		if boolArg(item, "uncertain") {
			lines = append(lines, "先核实是否已经收到；明确补发可能重复，不会自动重放未知写入。")
		}
		if manage && contains([]string{"pending", "uncertain"}, stringArg(item, "state")) {
			lines = append(lines, "明确补发：`/retry-output "+stringArg(item, "id")+"`")
		}
	}
	lines = append(lines, "", "补发只处理已保存的输出，不重新调用模型或执行原任务。")
	return strings.Join(lines, "\n"), true, nil
}
func (g *Gateway) command(ctx context.Context, r core.Room, m core.Message) error {
	cmd, arg := splitCommand(m.Text)
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
		var n int
		if e := g.store.DB.QueryRow("SELECT 1 FROM sessions WHERE room=? AND id=?", r.Key, arg).Scan(&n); e != nil {
			g.notify(r, "只能恢复本聊天列出的会话。", &m)
			return nil
		}
	}
	if e := g.stopRoom(ctx, r); e != nil {
		g.notify(r, "原生进程停止失败，尚未确认退出。当前会话保留，不能启动替代进程或切换会话；请先检查服务诊断并核实进程。", &m)
		return e
	}
	switch cmd {
	case "/resume":
		if _, e := g.store.Resume(r.Key, arg); e != nil {
			return e
		}
		g.notify(r, "已选择历史会话，旧运行未交付的补充已取消。下一条消息会恢复上下文；旧后台进程不会自动重启。", &m)
	case "/new":
		if _, e := g.store.DB.Exec("DELETE FROM inbox WHERE room=? AND status='background'", r.Key); e != nil {
			return e
		}
		if _, e := g.store.NewSession(r.Key); e != nil {
			return e
		}
		g.notify(r, "已新建本聊天会话，旧运行未交付的补充已取消。", &m)
	case "/stop":
		g.notify(r, "已停止本聊天运行、后台进程和待交付输入；未交付的补充已取消。上下文和会话 ID 保留，下条消息继续原会话；只有 /new 新建会话。", &m)
	}
	return nil
}
