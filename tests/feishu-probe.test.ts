import test from 'node:test';
import assert from 'node:assert/strict';
import { probeFeishuIdentity } from '../src/feishu-probe.ts';

for (const [name, response] of [
    ['redirect', () => new Response('private-response-body', { status: 302, headers: { location: 'https://untrusted.example' } })],
    ['HTTP failure', () => new Response('private-response-body', { status: 403 })],
    ['HTML', () => new Response('<html>private-response-body</html>')],
    ['missing status', () => Response.json({ tenant_access_token: 'private-response-body' })],
    ['missing token', () => Response.json({ code: 0 })],
    ['oversized response', () => new Response('private-response-body'.repeat(4000))]
] as const) {
    test(`identity probe rejects ${name} without forwarding credentials or provider content`, async t => {
        const fetch = t.mock.method(globalThis, 'fetch', async () => response());
        const result = await probeFeishuIdentity('cli_fixture', 'private-app-secret');
        assert.equal(result.auth, 'failed');
        assert.equal(result.identity, 'blocked');
        assert.equal(fetch.mock.callCount(), 1, 'failed token exchange must not continue to identity lookup');
        assert.doesNotMatch(JSON.stringify(result), /private-response-body|private-app-secret/);
    });
}

test('identity probe does not expose thrown network errors or accept a bot-less response', async t => {
    const fetch = t.mock.method(globalThis, 'fetch', async () => { throw new Error('private-network-detail'); });
    let result = await probeFeishuIdentity('cli_fixture', 'private-app-secret');
    assert.equal(result.auth, 'failed');
    assert.doesNotMatch(JSON.stringify(result), /private-network-detail|private-app-secret/);
    let calls = 0;
    fetch.mock.mockImplementation(async () => Response.json(++calls === 1
        ? { code: 0, tenant_access_token: 'private-access-token' } : { code: 0, bot: {} }));
    result = await probeFeishuIdentity('cli_fixture', 'private-app-secret');
    assert.equal(result.auth, 'passed');
    assert.equal(result.identity, 'failed');
    assert.doesNotMatch(JSON.stringify(result), /private-access-token|private-app-secret/);
});
