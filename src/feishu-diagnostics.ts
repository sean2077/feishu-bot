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
function own(value: object, key: string): unknown {
    try { const property = Object.getOwnPropertyDescriptor(value, key); return property && 'value' in property ? property.value : undefined; }
    catch { return undefined; }
}
/** Bounded metadata only: never stringify an SDK error or evaluate its getters. */
export function feishuDiagnostic(...values: unknown[]): FeishuDiagnostic {
    const queue = values.slice(0, 8).map(value => ({ value, depth: 0 })), seen = new Set<object>();
    let sdkCode: string | undefined, transportCode: string | undefined, httpStatus: number | undefined, apiCode: number | undefined, label: Category | undefined;
    for (let i = 0; i < queue.length && i < 32; i++) {
        const { value, depth } = queue[i]!;
        if (typeof value === 'string') { label ??= labels.get(value); continue; }
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
    return { category, ...(sdkCode ? { code: sdkCode } : {}), ...(transportCode ? { transportCode } : {}),
        ...(httpStatus !== undefined ? { httpStatus } : {}), ...(apiCode !== undefined ? { apiCode } : {}), summary, nextStep };
}
export function feishuSdkLogger(log: Log) {
    const emit = (event: string, values: unknown[]) => {
        try { log(event, feishuDiagnostic(...values)); } catch { /* Logging must not interrupt delivery or SDK cleanup. */ }
    };
    return { debug: () => {}, trace: () => {}, info: () => {},
        warn: (...values: unknown[]) => emit('feishu_warning', values),
        error: (...values: unknown[]) => emit('feishu_sdk_error', values) };
}
