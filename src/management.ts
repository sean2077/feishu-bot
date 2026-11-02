import type { Room, Session, Worker } from './types.ts';
import type { Store } from './store.ts';
import { usageReply } from './usage.ts';

interface RuntimeView {
    session: Session;
    worker?: Worker;
    live: boolean;
    busy: boolean;
    background: number;
    pinned: boolean;
    compacting?: string;
    stopUnconfirmed?: boolean;
}
interface Context {
    room: Room;
    actor: string;
    store: Store;
    runtime?: RuntimeView;
    approvals: number;
}
interface Inputs { queued: number; sent: number; consumed: number; uncertain: number; }
export interface StatusView {
    project: string;
    session?: string;
    nativeSessionId?: string;
    state: 'not_started' | 'suspended' | 'working' | 'connected' | 'termination_unconfirmed';
    phase: string;
    background: number | null;
    monitorPinned: boolean;
    pendingOrUnconfirmed: number;
    inputs: Inputs;
    approvals: number;
    pendingOutputs: number;
}
const inline = (value: string): string => value.replace(/[\r\n`<>\\*_\[\]]/g, ' ').slice(0, 200);
const count = (store: Store, sql: string, room: string): number => Number(store.db.prepare(sql).get(room)?.n ?? 0);
const pendingOutputCount = (store: Store, room: string): number => count(store,
    "SELECT count(*) AS n FROM outbox WHERE room=? AND state NOT IN ('done','cancelled')", room);
function selectedSession(store: Store, room: string): Session | undefined {
    const row = store.db.prepare("SELECT id,room,cc,state,created FROM sessions WHERE room=? AND state='open'").get(room);
    return row ? { id: String(row.id), roomKey: String(row.room), ccId: row.cc ? String(row.cc) : undefined,
        state: 'open', createdAt: Number(row.created) } : undefined;
}
function statusView(ctx: Context): StatusView {
    const { store, room, runtime: slot, approvals } = ctx;
    const saved = slot?.session ?? selectedSession(store, room.key);
    const inputs: Inputs = { queued: 0, sent: 0, consumed: 0, uncertain: 0 };
    for (const row of store.db.prepare("SELECT status,count(*) AS n FROM inbox WHERE room=? AND status IN ('queued','sent','consumed','uncertain') GROUP BY status").all(room.key))
        inputs[row.status as keyof Inputs] = Number(row.n);
    const pendingOutputs = pendingOutputCount(store, room.key);
    const state = slot?.stopUnconfirmed ? 'termination_unconfirmed' : slot ? (slot.busy ? 'working' : 'connected') : saved ? 'suspended' : 'not_started';
    const phase = slot?.stopUnconfirmed ? 'termination_unconfirmed' : slot && !slot.live ? 'stopping'
        : slot && !slot.worker ? 'starting' : slot?.compacting ? 'compacting' : approvals ? 'awaiting_answer'
        : slot?.busy ? 'working' : pendingOutputs ? 'delivery_pending' : inputs.queued ? 'queued'
        : slot?.pinned || (slot?.background ?? 0) > 0 ? 'background'
        : slot ? (slot.background < 0 ? 'connected_unknown' : 'idle') : state;
    return { project: room.projectId, session: saved?.id, nativeSessionId: saved?.ccId, state, phase,
        background: slot ? slot.background < 0 ? null : slot.background : 0, monitorPinned: slot?.pinned ?? false,
        pendingOrUnconfirmed: inputs.queued + inputs.sent + inputs.consumed + inputs.uncertain,
        inputs, approvals, pendingOutputs };
}
const phases: Record<string, string> = {
    not_started: '尚未启动', suspended: '已暂停运行，保留会话', starting: '正在启动原生会话',
    stopping: '正在停止，等待退出确认', termination_unconfirmed: '停止未确认，需要维护者检查',
    compacting: '正在整理原生上下文', awaiting_answer: '等待审批或回答', working: '处理中',
    delivery_pending: '有输出尚未完整送达', queued: '输入已保存，等待运行',
    background: '原生后台工作或监控中', connected_unknown: '已连接，后台状态尚未确认', idle: '空闲，可继续对话'
};
export function formatStatus(value: StatusView, canManage: boolean): string {
    const lines = ['**本聊天状态**', `状态：${phases[value.phase] ?? '未知，请检查服务诊断'}`, `项目：${inline(value.project)}`,
        `当前会话：${value.session ? `\`${inline(value.session)}\`` : '尚无会话'}`,
        `原生会话：${value.nativeSessionId ? `\`${inline(value.nativeSessionId)}\`` : '尚未建立'}`,
        '', `输入 · 排队：${value.inputs.queued} · 已交给原生：${value.inputs.sent} · 已消费待结果：${value.inputs.consumed} · 结果未知：${value.inputs.uncertain}`,
        `等待审批或回答：${value.approvals} · 待交付输出：${value.pendingOutputs}`,
        `后台活动：${value.background === null ? '未知（不会据此回收进程）' : value.background} · 监控保留：${value.monitorPinned ? '是' : '否'}`];
    if (value.phase === 'termination_unconfirmed') lines.push('', '不会启动替代进程或切换会话；请维护者核实服务诊断与进程状态。');
    else if (value.phase === 'suspended') lines.push('', '下条消息继续原会话，无需 /new。');
    else if (value.phase === 'not_started') lines.push('', '发送一条消息即可开始；群聊请明确 @ 机器人。');
    if (value.approvals) lines.push('请在对应卡片操作；每位成员的追问选择独立，工具审批仍限维护者。');
    if (value.inputs.uncertain) lines.push('结果未知的输入不会自动重放，请先核实外部结果，勿直接重复执行。');
    if (value.pendingOutputs) lines.push('用 /outputs 查看尚未送达的回复或文件，不必让模型重新执行任务。');
    lines.push('', canManage ? '管理：/stop 停止并保留上下文；/sessions 查看或恢复历史。' : '查看：/sessions 历史会话；/outputs 待交付记录。停止、切换和补发请联系维护者。');
    return lines.join('\n');
}
function timeLabel(at: number): string {
    return Number.isFinite(at) && Math.abs(at) <= 8.64e15 ? new Date(at).toISOString().replace('T', ' ').slice(0, 19) + ' UTC' : '时间未知';
}
const deliveryReasons: Record<string, string> = {
    'An agent file has not been confirmed delivered; the reply is not complete.': '本轮主动发送的文件尚未送达，回复需等待文件交付。',
    'File receipt saved; local delivery completion is pending.': '文件回执已保存，等待完成本地交付记录。',
    'Agent file upload is unconfirmed; an explicit retry may duplicate a previously accepted file.': '文件上传结果未知，先核实聊天中是否已经收到。',
    'Full-answer file delivery is unconfirmed; explicit retry may duplicate a previously accepted message.': '完整回答文件的发送结果未知，先核实是否已经收到。',
    'Cannot read or prepare the file; check the data directory permissions and capacity.': '本地文件准备失败，请检查数据目录权限和磁盘空间。',
    'Reply target saved; local delivery completion is pending.': '回复消息 ID 已保存，等待继续交付或完成本地记录。',
    'Reply card delivery is unconfirmed.': '回复卡片发送结果未知，先核实聊天中的消息。'
};
/** Read-only presentation. Never creates a session, changes delivery state or calls the native runtime. */
export function managementReply(command: string, arg: string, ctx: Context): string | undefined {
    if (!['/help', '/status', '/sessions', '/outputs', '/usage'].includes(command)) return undefined;
    if (arg && (arg !== '--json' || command === '/help')) return `用法：${command}${command === '/help' ? '' : ' [--json]'}`;
    const { store, room, actor } = ctx;
    const manage = room.maintainers.includes(actor) || room.dmUser === actor;
    if (command === '/help') return [
        '**日常对话**', '群里请明确 @ 机器人；私聊直接发送。忙时普通消息仍交给原生会话处理。',
        '/btw 正文、/ps 正文：仅补充当前前台任务，不新建任务，不代替审批回答；接收确认不代表模型已读取。',
        '', '**查看与回答**', '/status 当前工作、待回答事项及交付情况', '/sessions 本聊天历史会话', '/outputs 尚未完整送达的回复和文件', '/usage 当前选中会话的 SDK 用量，不估造费用或上下文占用', '/answer 请求ID 回答（多题也可使用卡片）',
        '', '**会话管理**', '/stop 停止运行，保留上下文；下条消息继续', '/compact [说明] 在轮次边界执行原生压缩', '/new 明确新建会话', '/resume 会话ID 选择本聊天历史', '/retry-output 输出ID 明确补发；结果未知时可能重复',
        manage ? '你可以管理本聊天会话。工具审批仍限项目维护者。' : '新建、恢复、停止、压缩和补发限项目维护者；私聊本人可管理自己的会话。',
        '', '排障需要机器数据时，可用 /status --json、/sessions --json、/outputs --json、/usage --json。'
    ].join('\n');
    if (command === '/usage') return usageReply(store, room, arg);
    if (command === '/status') {
        const view = statusView(ctx);
        return arg === '--json' ? JSON.stringify(view, null, 2) : formatStatus(view, manage);
    }
    if (command === '/sessions') {
        const selected = selectedSession(store, room.key), recent = store.listSessions(room.key);
        const total = count(store, 'SELECT count(*) AS n FROM sessions WHERE room=?', room.key);
        const sessions = selected && !recent.some(s => s.id === selected.id) ? [selected, ...recent] : recent;
        if (arg === '--json') return JSON.stringify({ total, recentLimit: 20, current: selected?.id,
            sessions: sessions.map(s => ({ id: s.id, state: s.state, createdAt: s.createdAt, current: s.id === selected?.id })) }, null, 2);
        if (!sessions.length) return '尚无历史会话。发送消息后会自动建立；查看列表不会启动模型。';
        const lines = ['**本聊天会话**', `共 ${total} 个；显示最近 20 个，并保留当前会话。`, '时间为 UTC；会话 ID 可直接复制。'];
        for (const s of sessions) lines.push('', `${s.id === selected?.id ? '当前会话' : s.state === 'stopped' ? '已停止' : '历史会话'} · ${timeLabel(s.createdAt)}\n\`${inline(s.id)}\``);
        lines.push('', manage ? '恢复：/resume 会话ID。仅下条消息恢复上下文，旧后台工作不会自动重启。' : '恢复历史会话请联系项目维护者。');
        return lines.join('\n');
    }
    const rows = store.db.prepare("SELECT id,state,file_state,delivery_error,kind FROM outbox WHERE room=? AND state NOT IN ('done','cancelled') ORDER BY at DESC,id DESC LIMIT 20").all(room.key);
    const items = rows.map(row => ({ id: String(row.id), state: String(row.state),
        stage: row.kind === 'file' || row.file_state !== 'none' && row.file_state !== 'done' ? 'file' : 'reply',
        uncertain: row.state === 'uncertain' || row.file_state === 'uncertain',
        ...(row.delivery_error ? { detail: deliveryReasons[String(row.delivery_error)] ?? '交付尚未完成，请查看服务诊断。' } : {}) }));
    if (arg === '--json') return JSON.stringify(items, null, 2);
    if (!items.length) return '本聊天没有待交付输出。这里只表示交付记录已清空，不代表所有输入都已执行完成；输入状态见 /status。';
    const lines = ['**待交付输出**', `共 ${pendingOutputCount(store, room.key)} 条，显示最近 20 条。`];
    for (const item of items) {
        const state = item.uncertain ? '结果未知' : item.state === 'pending' ? '等待交付' : item.state === 'sending' ? '正在发送' : item.state === 'preparing' ? '正在准备' : '状态未知';
        lines.push('', `${item.stage === 'file' ? '文件' : '回复'} · ${state}\n\`${inline(item.id)}\``);
        if (item.detail) lines.push(item.detail);
        if (item.uncertain) lines.push('先核实是否已经收到；明确补发可能重复，不会自动重放未知写入。');
        else if (item.state === 'pending') lines.push('服务会按已有恢复规则继续处理；文件依赖或渠道故障可能阻止完成。');
        else lines.push('等待当前交付结束，请勿重复操作。');
        if (['pending', 'uncertain'].includes(item.state)) lines.push(manage ? `明确补发：\`/retry-output ${inline(item.id)}\`` : '需要补发时请联系项目维护者。');
    }
    lines.push('', '补发只处理已保存的输出，不重新调用模型或执行原任务。');
    return lines.join('\n');
}
