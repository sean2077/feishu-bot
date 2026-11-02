import type { Store } from './store.ts';
import type { Room, Session, WorkerEvent } from './types.ts';

export const tokenFields = ['input_tokens', 'output_tokens', 'cache_read_input_tokens', 'cache_creation_input_tokens'] as const;
type Field = typeof tokenFields[number];
type Tokens = Record<Field, number | null>;
type Result = Extract<WorkerEvent, { type: 'result' }>;
const validNumber = (value: unknown): number | null => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : null;
export function reportedTokens(value: unknown): Tokens {
    const data = value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
    return Object.fromEntries(tokenFields.map(field => [field, validNumber(data[field])])) as Tokens;
}
function reportedModels(value: unknown): string[] {
    return Array.isArray(value) ? [...new Set(value.filter((v): v is string => typeof v === 'string' && /^[a-zA-Z0-9_.:/-]{1,200}$/.test(v)))].sort().slice(0, 32) : [];
}
/** Called in the result audit transaction. Only native result IDs authorize deduplicated accumulation. */
export function recordUsage(store: Store, room: Room, session: Session, event: Result): 'new' | 'duplicate' | 'unidentified' {
    if (typeof event.resultId !== 'string' || !/^[a-zA-Z0-9_-]{1,200}$/.test(event.resultId)) {
        store.db.prepare('UPDATE usage_coverage SET unidentified=1 WHERE room=? AND session=?').run(room.key, session.id);
        return 'unidentified';
    }
    const usage = reportedTokens(event.usage), models = reportedModels(event.models);
    const inserted = store.db.prepare(`INSERT OR IGNORE INTO usage_results
        (session,result_id,room,at,kind,success,models,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens)
        VALUES(?,?,?,?,?,?,?,?,?,?,?)`).run(session.id, event.resultId, room.key, Date.now(), event.compact ? 'compact' : 'turn', Number(event.success),
            JSON.stringify(models), ...tokenFields.map(field => usage[field]));
    return inserted.changes ? 'new' : 'duplicate';
}
export function usageReport(store: Store, room: Room) {
    const selected = store.db.prepare("SELECT id FROM sessions WHERE room=? AND state='open'").get(room.key);
    const session = selected ? String(selected.id) : null;
    const coverage = session ? store.db.prepare('SELECT started,prior_unknown,unidentified FROM usage_coverage WHERE room=? AND session=?').get(room.key, session) : undefined;
    const latest = session ? store.db.prepare('SELECT * FROM usage_results WHERE room=? AND session=? ORDER BY at DESC,rowid DESC LIMIT 1').get(room.key, session) : undefined;
    const summary = session ? store.db.prepare(`SELECT count(*) AS results, ${tokenFields.map(f => `count(${f}) AS ${f}_reports,total(${f}) AS ${f}_sum`).join(',')}
        FROM usage_results WHERE room=? AND session=?`).get(room.key, session) : undefined;
    const results = Number(summary?.results ?? 0);
    const usage = {} as Tokens, subtotal = {} as Tokens, missing = {} as Record<Field, number>;
    for (const field of tokenFields) {
        const reports = Number(summary?.[`${field}_reports`] ?? 0), sum = validNumber(summary?.[`${field}_sum`]);
        missing[field] = results - reports;
        subtotal[field] = reports ? sum : null;
        usage[field] = results > 0 && reports === results && !coverage?.unidentified ? sum : null;
    }
    const modelRows = session ? store.db.prepare(`SELECT DISTINCT j.value AS model FROM usage_results u,json_each(u.models) j
        WHERE u.room=? AND u.session=? ORDER BY j.value LIMIT 65`).all(room.key, session) : [];
    return {
        session, scope: 'sdk_result_main_loop', since: coverage ? Number(coverage.started) : null,
        historyIncomplete: session ? !coverage || !!coverage.prior_unknown : false,
        unidentifiedResults: !!coverage?.unidentified,
        latest: latest ? { resultId: String(latest.result_id), at: Number(latest.at), kind: String(latest.kind), success: !!latest.success,
            models: JSON.parse(String(latest.models)).length ? JSON.parse(String(latest.models)) as string[] : null,
            usage: reportedTokens(latest) } : null,
        cumulative: { results, usage, reportedSubtotal: subtotal, missing,
            models: modelRows.length ? modelRows.slice(0, 64).map(row => String(row.model)) : null, modelsTruncated: modelRows.length > 64 },
        cost: null, contextOccupancy: null
    };
}
export function usageReply(store: Store, room: Room, arg: string): string {
    if (arg && arg !== '--json') return '用法：/usage [--json]';
    const report = usageReport(store, room);
    if (arg === '--json') return JSON.stringify(report, null, 2);
    if (!report.session) return '本聊天尚无会话或用量记录。查询不会创建会话、启动 Worker 或调用模型。';
    const labels: Record<Field, string> = { input_tokens: '输入（不含缓存读写）', output_tokens: '输出', cache_read_input_tokens: '缓存读取', cache_creation_input_tokens: '缓存创建' };
    const amount = (v: number | null) => v === null ? '未知' : String(v);
    const when = (at: number | null) => at === null ? '未知' : new Date(at).toISOString().replace('T', ' ').replace('.000Z', ' UTC');
    const lines = ['**本会话使用量**', `会话：\`${report.session}\``, `统计起点：${when(report.since)}`,
        '口径：SDK result.usage，主循环每轮的已报告用量，不含子代理；不是当前上下文占用或账单。'];
    if (report.historyIncomplete) lines.push('旧历史未完整采集，下面仅累计统计起点后的观测，不能当作完整历史总量。');
    if (report.unidentifiedResults) lines.push('存在未提供稳定结果 ID 的结果，未纳入累计；总量未知，已报告小计仅供核对。');
    if (!report.latest) lines.push('', '尚无可去重的原生结果用量。未报告字段保持未知，不填零。');
    else {
        lines.push('', `**最近一轮** · ${report.latest.kind === 'compact' ? '原生压缩' : '任务结果'} · ${when(report.latest.at)}`,
            `状态：${report.latest.success ? '原生结果成功（交付另见 /outputs）' : '原生结果失败，已消耗用量仍保留'}`,
            `SDK 报告的主循环模型：${report.latest.models?.join('、') ?? '未知'}`);
        for (const field of tokenFields) lines.push(`${labels[field]}：${amount(report.latest.usage[field])}`);
    }
    lines.push('', `**累计观测** · ${report.cumulative.results} 个去重结果`,
        `涉及的主循环模型：${report.cumulative.models?.join('、') ?? '未知'}${report.cumulative.modelsTruncated ? '（仅显示前 64 个）' : ''}`);
    for (const field of tokenFields) lines.push(`${labels[field]}：${amount(report.cumulative.usage[field])}` +
        (report.cumulative.missing[field] ? `；${report.cumulative.missing[field]} 轮未报告，已报告小计 ${amount(report.cumulative.reportedSubtotal[field])}` : ''));
    lines.push('', '同轮多模型只列出归属，不把混合用量猜分到单个模型。不累计 modelUsage 的累计快照，不估造费用；进行中或未返回结果的消耗尚不可知。');
    return lines.join('\n');
}
