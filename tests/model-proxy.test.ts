import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { once } from 'node:events';
import { parseConfig } from '../src/config.ts';
import { ModelBudget, startModelProxy } from '../src/model-proxy.ts';
import { rawConfig } from './helpers.ts';
test('model proxy fixes upstream/model scope and keeps company key out of worker capability', async () => {
    const received: {
        path: string;
        authorization: string;
    }[] = [];
    const server = http.createServer((req, res) => { received.push({ path: req.url!, authorization: String(req.headers.authorization) }); req.resume(); req.on('end', () => { res.setHeader('content-type', 'application/json'); res.end('{"content":[{"type":"text","text":"OK"}]}'); }); });
    server.listen(0, '127.0.0.1');
    await once(server, 'listening');
    const addr = server.address();
    assert.ok(addr && typeof addr === 'object');
    process.env.TEST_MODEL_SECRET = 'company-secret';
    const config = parseConfig({ ...rawConfig('/tmp/x'), model: { baseUrl: `http://127.0.0.1:${addr.port}/anthropic`, allowHttp: true, tokenEnv: 'TEST_MODEL_SECRET', model: 'deepseek' } });
    const proxy = await startModelProxy(config, new ModelBudget(2));
    try {
        assert.notEqual(proxy.token, 'company-secret');
        const call = (path: string, model = 'deepseek', token = proxy.token) => fetch(proxy.url! + path, { method: 'POST', headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json' }, body: JSON.stringify({ model, messages: [] }) });
        assert.equal((await call('/v1/messages', 'deepseek', 'bad')).status, 401);
        assert.equal((await call('/admin')).status, 403);
        assert.equal((await call('/v1/messages', 'unexpected')).status, 403);
        const response = await call('/v1/messages');
        assert.equal(response.status, 200);
        await response.text();
        assert.deepEqual(received, [{ path: '/anthropic/v1/messages', authorization: 'Bearer company-secret' }]);
    }
    finally {
        await proxy.close();
        server.closeAllConnections();
        await new Promise<void>(r => server.close(() => r()));
        delete process.env.TEST_MODEL_SECRET;
    }
});
