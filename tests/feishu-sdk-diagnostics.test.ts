import test from 'node:test';
import assert from 'node:assert/strict';
import { feishuTransport } from '../src/feishu.ts';
import { feishuDiagnostic } from '../src/feishu-diagnostics.ts';
import { createLarkChannel } from '@larksuite/channel';
import { bindFeishuInbound } from '../src/feishu-events.ts';
import { fixture } from './helpers.ts';
import { hash } from '../src/util.ts';

test('public policy rejection records only hashed metadata without admitting content or attachments', async () => {
    const f = await fixture();
    const channel = createLarkChannel({ appId: 'cli_fixture', appSecret: 'fixture-not-real',
        policy: { groupAllowlist: ['oc_allowed'], dmMode: 'allowlist', dmAllowlist: ['ou_allowed'] } });
    const entries: { event: string; detail: object }[] = [];
    let closing = false;
    const sdk = channel as unknown as { registerDispatcherHandlers(): void; dispatcher: { handles: Map<string, (event: unknown) => Promise<unknown>> } };
    bindFeishuInbound(channel, f.gateway, (event, detail) => entries.push({ event, detail }), () => closing);
    sdk.registerDispatcherHandlers();
    const raw = (id: string) => ({ sender: { sender_id: { open_id: 'PRIVATE_USER' }, sender_type: 'user' }, message: {
        message_id: id, chat_id: 'PRIVATE_CHAT', chat_type: 'group', message_type: 'post', create_time: String(Date.now()),
        content: JSON.stringify({ content: [[{ tag: 'text', text: 'PRIVATE_BODY' }, { tag: 'img', image_key: 'PRIVATE_IMAGE' }]] }) } });
    try {
        await sdk.dispatcher.handles.get('im.message.receive_v1')!(raw('PRIVATE_MESSAGE'));
        assert.deepEqual(entries, [{ event: 'feishu_policy_rejected', detail: { reason: 'group_not_allowed',
            message: hash('PRIVATE_MESSAGE').slice(0, 16), chat: hash('PRIVATE_CHAT').slice(0, 16), sender: hash('PRIVATE_USER').slice(0, 16) } }]);
        assert.equal(JSON.stringify(entries).includes('PRIVATE_'), false);
        assert.equal(f.workers.length, 0);
        assert.equal(f.transport.sent.length, 0);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n, 0);
        closing = true;
        await sdk.dispatcher.handles.get('im.message.receive_v1')!(raw('PRIVATE_LATE_MESSAGE'));
        assert.equal(entries.length, 1, 'closed diagnostic must not retain late rejected inputs');
    } finally { await channel.disconnect(); await f.close(); }
});

test('locked Channel SDK identity failures retain useful categories without exposing raw provider data', async () => {
    const f = await fixture();
    const env = 'FEISHU_DIAGNOSTIC_FIXTURE_SECRET', previous = process.env[env];
    f.config.appSecretEnv = env;
    process.env[env] = 'fixture-not-a-real-secret';
    try {
        const cases = [
            { raw: { response: { status: 403, data: { code: 99991401, msg: 'PRIVATE_BODY' } } }, expected: 'permission_denied' },
            { raw: { response: { status: 429, data: { msg: 'PRIVATE_BODY' } } }, expected: 'rate_limited' },
            { raw: { code: 'ETIMEDOUT', message: 'PRIVATE_BODY' }, expected: 'send_timeout' },
            { raw: new Error('PRIVATE_BODY'), expected: 'not_connected' }
        ];
        for (const item of cases) {
            const { channel } = feishuTransport(f.config, () => {});
            let requests = 0;
            // Public connect() must stop at identity lookup; no WebSocket or live HTTP is reached.
            Object.defineProperty(channel.rawClient, 'request', { value: async () => { requests++; throw item.raw; } });
            try {
                await assert.rejects(channel.connect(), error => {
                    const detail = feishuDiagnostic(error);
                    assert.equal(detail.category, item.expected);
                    assert.equal(detail.code, item.expected);
                    assert.ok(!JSON.stringify(detail).includes('PRIVATE_BODY'));
                    assert.ok(!JSON.stringify(detail).includes('fixture-not-a-real-secret'));
                    return true;
                });
                assert.equal(requests, 1, 'diagnostics must not introduce retries');
                assert.equal(channel.getConnectionStatus(), undefined, 'identity failure must not start WebSocket');
            } finally { await channel.disconnect(); }
        }
        assert.equal(f.workers.length, 0);
        assert.equal(f.transport.sent.length, 0);
    } finally {
        if (previous === undefined) delete process.env[env]; else process.env[env] = previous;
        await f.close();
    }
});

test('production channel wires the structured logger through the actual SDK warning path', async () => {
    const f = await fixture();
    const env = 'FEISHU_WARNING_FIXTURE_SECRET', previous = process.env[env];
    process.env[env] = 'fixture-not-a-real-secret';
    f.config.appSecretEnv = env;
    const entries: { event: string; detail: object }[] = [];
    const { channel } = feishuTransport(f.config, (event, detail) => entries.push({ event, detail }));
    try {
        // Overwriting a handler emits an SDK warning without connecting to any service.
        channel.on('message', async () => {});
        channel.on('message', async () => {});
        const warning = entries.find(e => e.event === 'feishu_warning');
        assert.ok(warning);
        assert.deepEqual(warning.detail, feishuDiagnostic(), 'unknown warning text is not forwarded');
        assert.equal(channel.getConnectionStatus(), undefined);
        assert.equal(f.workers.length, 0);
    } finally {
        await channel.disconnect();
        if (previous === undefined) delete process.env[env]; else process.env[env] = previous;
        await f.close();
    }
});

test('production channel enables native pong and handshake watchdogs without unsafe optional keepalive', async () => {
    const f = await fixture(), env = 'WATCHDOG_FIXTURE_SECRET', previous = process.env[env];
    process.env[env] = 'fixture-not-real'; f.config.appSecretEnv = env;
    let captured: Parameters<typeof createLarkChannel>[0] | undefined;
    const { channel } = feishuTransport(f.config, () => {}, options => { captured = options; return createLarkChannel(options); });
    try {
        assert.equal(captured!.wsConfig?.pingTimeout, 30, 'the SDK field is in seconds after ping, not milliseconds');
        assert.equal(captured!.handshakeTimeoutMs, 10000);
        assert.notEqual(captured!.keepalive?.enabled, true, 'locked keepalive may reopen a consumer after disconnect during HTTP probe');
        assert.equal(captured!.safety?.chatQueue?.cardActions, 'separate');
        assert.equal(captured!.outbound?.retry?.maxAttempts, 1);
        assert.equal(captured!.loggerLevel, 4, 'debug metadata is filtered by the strict logger adapter');
    } finally {
        await channel.disconnect(); await f.close();
        if (previous === undefined) delete process.env[env]; else process.env[env] = previous;
    }
});

test('public card callback stages record timing without logging action values or identities', async () => {
    const f = await fixture(), channel = createLarkChannel({ appId: 'cli_fixture', appSecret: 'fixture-not-real' });
    const entries: { event: string; detail: object }[] = [];
    const sdk = channel as unknown as { registerDispatcherHandlers(): void; dispatcher: { handles: Map<string, (event: unknown) => Promise<unknown>> } };
    bindFeishuInbound(channel, f.gateway, (event, detail) => entries.push({ event, detail }), () => false);
    sdk.registerDispatcherHandlers();
    try {
        await sdk.dispatcher.handles.get('card.action.trigger')!({
            context: { open_message_id: 'PRIVATE_MESSAGE', open_chat_id: 'PRIVATE_CHAT' },
            operator: { open_id: 'PRIVATE_USER' }, action: { tag: 'button', value: { request: 'PRIVATE_REQUEST', arbitrary: 'PRIVATE_CONTENT' } }
        });
        assert.deepEqual(entries.map(item => item.event), ['card_callback_started', 'card_callback_completed']);
        assert.equal((entries[1]!.detail as { response: string }).response, 'silent');
        assert.ok((entries[1]!.detail as { elapsedMs: number }).elapsedMs >= 0);
        assert.equal(JSON.stringify(entries).includes('PRIVATE_'), false);
    } finally { await channel.disconnect(); await f.close(); }
});
