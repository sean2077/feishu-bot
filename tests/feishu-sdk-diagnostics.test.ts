import test from 'node:test';
import assert from 'node:assert/strict';
import { feishuTransport } from '../src/feishu.ts';
import { feishuDiagnostic } from '../src/feishu-diagnostics.ts';
import { fixture } from './helpers.ts';

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
