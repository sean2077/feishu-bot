type Log = (event: string, detail: object) => void;
const guidance = {
    permission_denied: ['认证或权限检查未通过', '核实应用凭据、发布状态和对应权限；身份检查可显式运行 doctor --probe-feishu。'],
    rate_limited: ['飞书请求受到限流', '检查请求频率与待交付输出，勿通过重复发送原任务绕过限流。'],
    send_timeout: ['请求超时，外部结果可能未确认', '先检查网络和 /outputs，再核实飞书是否已经收到；勿直接重做任务。'],
    not_connected: ['连接或机器人身份查询未完成', '检查网络、代理、应用身份与发布状态；这不单独证明是网络故障。'],
    network_error: ['网络连接未正常完成', '检查 DNS、代理和连接状态；涉及发送时先核实外部结果。'],
    format_error: ['SDK 报告请求格式被拒绝', '核实对应消息或卡片格式与服务版本；不要把失败当作成功交付。'],
    target_revoked: ['SDK 报告目标不存在或已失效', '核实目标聊天、消息及应用可见范围；不要自动创建替代目标。'],
    ssrf_blocked: ['资源地址被安全策略阻止', '核实资源来源，不要关闭地址检查来绕过拒绝。'],
    http_error: ['飞书接口返回错误状态', '结合状态码检查接口与服务状态；未知写入结果仍需先核实。'],
    unclassified: ['SDK 未提供可识别的错误分类', '结合发生时间、doctor 和本聊天 /status、/outputs 检查；日志不包含原始错误正文。']
} as const;
type Category = keyof typeof guidance;
export interface FeishuDiagnostic {
    signal?: string;
    category: Category;
    code?: string;
    transportCode?: string;
    httpStatus?: number;
    apiCode?: number;
    summary: string;
    nextStep: string;
}
const sdkCodes = new Set(['permission_denied', 'rate_limited', 'send_timeout', 'not_connected', 'format_error', 'target_revoked', 'ssrf_blocked', 'unknown']);
const networkCodes = new Set(['ECONNRESET', 'ECONNREFUSED', 'ENOTFOUND', 'EAI_AGAIN', 'ETIMEDOUT', 'ECONNABORTED']);
const apiCategories = new Map<number, Category>([
    [99991400, 'permission_denied'], [99991401, 'permission_denied'],
    [230001, 'format_error'], [230002, 'format_error'],
    [230011, 'target_revoked'], [230017, 'target_revoked'], [230020, 'target_revoked']
]);
const labels = new Map<string, Category>([
    ['channel: network unreachable', 'network_error'],
    ['channel: keepalive ws-stuck', 'not_connected'],
    ['channel: keepalive force-reconnect failed', 'not_connected']
]);
const fixedSignals = new Map<string, string>([
    ['channel: network unreachable', 'network_unreachable'], ['channel: keepalive ws-stuck', 'ws_stuck'],
    ['channel: keepalive force-reconnect', 'ws_force_reconnect'], ['channel: keepalive force-reconnect failed', 'ws_force_reconnect_failed'],
    ['channel: keepalive tick failed', 'keepalive_failed'], ['channel: keepalive wake-up', 'host_wakeup'],
    ['channel: keepalive recovered', 'ws_recovered'], ['channel: network reachable again', 'network_recovered'],
    ['verification failed event', 'event_verification_failed'], ['no card.action.trigger handle', 'card_handler_missing'],
    ['safety: action handler threw', 'card_handler_failed'],
    ['reconnect', 'ws_reconnecting'], ['reconnect success', 'ws_reconnected'], ['ws client ready', 'ws_ready'],
    ['ws connect success', 'ws_connected'], ['connect failed', 'ws_connect_failed'], ['ws connect failed', 'ws_connect_failed'],
    ['ws error', 'ws_error'], ['send data failed', 'ws_ack_failed'], ['new WebSocket error', 'ws_constructor_failed'],
    ['client closed', 'ws_closed'], ['ws client closed manually', 'ws_closed_manually'], ['ws client closed manually (force)', 'ws_closed_forced']
]);
function safeSignal(text: string): string | undefined {
    const fixed = fixedSignals.get(text);
    if (fixed) return fixed;
    // Never emit the suffix: it includes serialized card values and identities.
    if (text.startsWith('safety: drop duplicate action ')) return 'card_duplicate_dropped';
    if (text.startsWith('safety: drop in-flight action ')) return 'card_inflight_dropped';
    if (/^no pong\/inbound within [0-9.]+s of last ping, terminating to trigger reconnect$/.test(text)) return 'ws_pong_timeout';
    if (/^handshake timeout after [0-9]+ms$/.test(text)) return 'ws_handshake_timeout';
    return undefined;
}
function own(value: object, key: string): unknown {
    try { const property = Object.getOwnPropertyDescriptor(value, key); return property && 'value' in property ? property.value : undefined; }
    catch { return undefined; }
}
/** Bounded metadata only: never stringify an SDK error or evaluate its getters. */
export function feishuDiagnostic(...values: unknown[]): FeishuDiagnostic {
    const queue = values.slice(0, 8).map(value => ({ value, depth: 0 })), seen = new Set<object>();
    let sdkCode: string | undefined, transportCode: string | undefined, httpStatus: number | undefined, apiCode: number | undefined, label: Category | undefined, signal: string | undefined;
    for (let i = 0; i < queue.length && i < 32; i++) {
        const { value, depth } = queue[i]!;
        if (typeof value === 'string') { label ??= labels.get(value); signal ??= safeSignal(value); continue; }
        if (!value || typeof value !== 'object' || seen.has(value)) continue;
        seen.add(value);
        const code = own(value, 'code'), status = own(value, 'status');
        if (typeof code === 'string' && sdkCodes.has(code) && (!sdkCode || sdkCode === 'unknown')) sdkCode = code;
        if (typeof code === 'string' && networkCodes.has(code)) transportCode ??= code;
        if (typeof code === 'number' && apiCategories.has(code)) apiCode ??= code;
        if (typeof status === 'number' && Number.isInteger(status) && status >= 100 && status <= 599) httpStatus ??= status;
        if (depth >= 3) continue;
        // Fixed keys and bounded arrays, not arbitrary object traversal or a log of request data.
        let keys: string[];
        try { keys = Array.isArray(value) ? ['0', '1', '2', '3', '4', '5', '6', '7'] : ['cause', 'response', 'data']; }
        catch { continue; } // Revoked proxies have no readable diagnostic metadata.
        for (const key of keys) if (queue.length < 32) queue.push({ value: own(value, key), depth: depth + 1 });
    }
    const category: Category = sdkCode && sdkCode !== 'unknown' ? sdkCode as Category
        : apiCode !== undefined ? apiCategories.get(apiCode)!
        : httpStatus === 401 || httpStatus === 403 ? 'permission_denied'
        : httpStatus === 429 ? 'rate_limited'
        : transportCode === 'ETIMEDOUT' || transportCode === 'ECONNABORTED' ? 'send_timeout'
        : transportCode ? 'network_error' : label ?? (httpStatus !== undefined && httpStatus >= 400 ? 'http_error' : 'unclassified');
    const [summary, nextStep] = guidance[category];
    return { category, ...(signal ? { signal } : {}), ...(sdkCode ? { code: sdkCode } : {}), ...(transportCode ? { transportCode } : {}),
        ...(httpStatus !== undefined ? { httpStatus } : {}), ...(apiCode !== undefined ? { apiCode } : {}), summary, nextStep };
}
export function feishuSdkLogger(log: Log) {
    const emit = (event: string, values: unknown[]) => {
        try { log(event, feishuDiagnostic(...values)); } catch { /* Logging must not interrupt delivery or SDK cleanup. */ }
    };
    const signal = (...values: unknown[]) => {
        try { const diagnostic = feishuDiagnostic(...values); if (diagnostic.signal) log('feishu_sdk_signal', { signal: diagnostic.signal }); } catch { }
    };
    return { debug: signal, trace: () => {}, info: signal,
        warn: (...values: unknown[]) => emit('feishu_warning', values),
        error: (...values: unknown[]) => emit('feishu_sdk_error', values) };
}
