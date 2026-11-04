import test from 'node:test';
import assert from 'node:assert/strict';
import { feishuDiagnostic, feishuSdkLogger } from '../src/feishu-diagnostics.ts';

test('SDK diagnostics retain recognized categories and actionable fixed guidance', () => {
    for (const code of ['permission_denied', 'rate_limited', 'send_timeout', 'not_connected', 'format_error', 'target_revoked', 'ssrf_blocked']) {
        const detail = feishuDiagnostic({ code, message: 'PRIVATE_PROVIDER_BODY' });
        assert.equal(detail.category, code);
        assert.equal(detail.code, code);
        assert.ok(detail.summary.length > 0 && detail.nextStep.length > 0);
        assert.ok(!JSON.stringify(detail).includes('PRIVATE_PROVIDER_BODY'));
    }
    assert.match(feishuDiagnostic({ code: 'not_connected' }).nextStep, /不单独证明是网络故障/);
    assert.match(feishuDiagnostic({ code: 'send_timeout' }).nextStep, /先检查/);
});

test('SDK diagnostics inspect bounded error metadata, not request bodies or arbitrary codes', () => {
    const detail = feishuDiagnostic('untrusted log text', [{ cause: { response: {
        status: 403, data: { code: 99991401, msg: 'PRIVATE_API_BODY' },
        headers: { authorization: 'Bearer PRIVATE_HEADER' }
    } } }]);
    assert.equal(detail.category, 'permission_denied');
    assert.equal(detail.httpStatus, 403);
    // Direct wrappers also retain recognized platform codes.
    assert.equal(feishuDiagnostic({ response: { data: { code: 99991401 } } }).apiCode, 99991401);
    assert.equal(feishuDiagnostic({ status: 429 }).category, 'rate_limited');
    assert.equal(feishuDiagnostic({ code: 'ETIMEDOUT' }).category, 'send_timeout');
    assert.equal(feishuDiagnostic({ code: 'ENOTFOUND' }).category, 'network_error');
    assert.equal(feishuDiagnostic({ status: 503 }).category, 'http_error');
    assert.equal(feishuDiagnostic({ status: 400 }).category, 'http_error', 'raw HTTP status alone must not invent an SDK format diagnosis');
    const unknown = feishuDiagnostic({ code: 'PRIVATE_CODE', status: 999999, message: 'PRIVATE_ERROR', path: '/private/path', response: { data: { code: 123456789 } } });
    assert.equal(unknown.category, 'unclassified');
    assert.equal(unknown.code, undefined);
    assert.equal(unknown.apiCode, undefined);
    assert.equal(unknown.httpStatus, undefined);
    assert.ok(!JSON.stringify([detail, unknown]).includes('PRIVATE_'));
    assert.ok(!JSON.stringify(unknown).includes('/private/path'));
});

test('SDK diagnostics do not inspect accessors, stringify objects or recurse through cycles', () => {
    let reads = 0;
    const value = Object.create({ code: 'permission_denied' }) as Record<string, unknown>;
    for (const key of ['code', 'status', 'cause', 'response', 'data', 'message'])
        Object.defineProperty(value, key, { get() { reads++; throw new Error('must not inspect accessor'); } });
    value.toJSON = () => { reads++; throw new Error('must not stringify'); };
    assert.equal(feishuDiagnostic(value).category, 'unclassified');
    assert.equal(reads, 0);
    const cycle: Record<string, unknown> = { code: 'unknown' }; cycle.cause = cycle;
    assert.equal(feishuDiagnostic(cycle).category, 'unclassified');
    const revoked = Proxy.revocable({}, {}); revoked.revoke();
    assert.doesNotThrow(() => feishuDiagnostic(revoked.proxy));
    assert.doesNotThrow(() => feishuDiagnostic(new Proxy({}, { getOwnPropertyDescriptor() { throw new Error('unreadable'); } })));
    assert.equal(feishuDiagnostic({ cause: { cause: { cause: { cause: { code: 'permission_denied' } } } } }).category, 'unclassified');
    assert.equal(feishuDiagnostic(...Array(8).fill(null), { code: 'permission_denied' }).category, 'unclassified');
});

test('SDK warning labels are exact and unknown strings never become log content', () => {
    assert.equal(feishuDiagnostic('channel: network unreachable').category, 'network_error');
    assert.equal(feishuDiagnostic('channel: keepalive ws-stuck').category, 'not_connected');
    assert.equal(feishuDiagnostic('channel: network unreachable PRIVATE_BODY').category, 'unclassified');
    assert.equal(feishuDiagnostic('permission_denied Bearer PRIVATE_TOKEN').category, 'unclassified');
    assert.ok(!JSON.stringify(feishuDiagnostic('PRIVATE_BODY')).includes('PRIVATE_BODY'));
    assert.equal(feishuDiagnostic({ code: 'unknown', cause: { code: 'permission_denied' } }).category, 'permission_denied');
});

test('SDK logger keeps quiet levels silent and logging failures do not affect delivery', () => {
    const entries: { event: string; detail: object }[] = [];
    const logger = feishuSdkLogger((event, detail) => entries.push({ event, detail }));
    logger.debug(); logger.trace(); logger.info();
    assert.equal(entries.length, 0);
    logger.warn('untrusted PRIVATE_BODY', { code: 'rate_limited' });
    logger.error({ code: 'send_timeout', cause: new Error('PRIVATE_BODY') });
    assert.deepEqual(entries.map(e => e.event), ['feishu_warning', 'feishu_sdk_error']);
    assert.ok(!JSON.stringify(entries).includes('PRIVATE_BODY'));
    const broken = feishuSdkLogger(() => { throw new Error('sink unavailable'); });
    assert.doesNotThrow(() => broken.warn({ code: 'unknown' }));
    assert.doesNotThrow(() => broken.error(null));
});

test('SDK connection and card drop signals survive wrapped logs without exposing their suffixes', () => {
    const entries: { event: string; detail: object }[] = [];
    const logger = feishuSdkLogger((event, detail) => entries.push({ event, detail }));
    logger.debug(['safety: drop duplicate action PRIVATE_CARD_AND_VALUE']);
    logger.debug(['safety: drop in-flight action PRIVATE_CARD_AND_VALUE']);
    logger.info(['[ws]', 'reconnect']);
    logger.warn(['[ws]', 'no pong/inbound within 30s of last ping, terminating to trigger reconnect']);
    logger.warn(['no card.action.trigger handle']);
    logger.debug(['[ws]', 'get connect config success, ws url: https://PRIVATE_ENDPOINT']);
    logger.debug(['[ws]', 'receive message, data: PRIVATE_MESSAGE']);
    logger.info('PRIVATE_INFO');
    assert.deepEqual(entries.map(item => (item.detail as { signal: string }).signal),
        ['card_duplicate_dropped', 'card_inflight_dropped', 'ws_reconnecting', 'ws_pong_timeout', 'card_handler_missing']);
    assert.equal(JSON.stringify(entries).includes('PRIVATE_'), false);
});
