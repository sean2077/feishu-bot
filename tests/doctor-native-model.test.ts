import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseConfig } from '../src/config.ts';
import { runDoctor } from '../src/doctor.ts';

test('basic model probe uses the API model name while preserving native context configuration', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'doctor-native-model-'));
    const before = { ...process.env }, observed: string[] = [];
    const server = createServer(async (request, response) => {
        const chunks: Buffer[] = []; for await (const chunk of request) chunks.push(chunk);
        const body = JSON.parse(Buffer.concat(chunks).toString());
        observed.push(body.model);
        response.writeHead(200, { 'content-type': 'application/json' });
        // A short reasoning budget can return a valid but truncated thinking
        // block before there is any answer. The probe must allow an answer.
        response.end(JSON.stringify({ type: 'message', role: 'assistant', content: body.max_tokens < 256
            ? [{ type: 'thinking', thinking: 'fixture reasoning' }] : [{ type: 'text', text: 'OK' }],
            stop_reason: body.max_tokens < 256 ? 'max_tokens' : 'end_turn' }));
    });
    try {
        await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
        process.env.DOCTOR_NATIVE_APP = 'fixture-app'; process.env.DOCTOR_NATIVE_MODEL = 'fixture-model-token';
        const address = server.address(); assert.ok(address && typeof address !== 'string');
        for (const model of ['fixture-model[1m]', 'fixture-model[1M]', 'fixture-model[custom]']) {
            const config = parseConfig({ tenantId: 'fixture', appId: 'cli_fixture', appSecretEnv: 'DOCTOR_NATIVE_APP', dataDir: dir,
                model: { baseUrl: `http://127.0.0.1:${address.port}`, allowHttp: true, tokenEnv: 'DOCTOR_NATIVE_MODEL', model },
                projects: [{ id: 'p', maintainers: ['ou_fixture'] }], dms: [{ userId: 'ou_fixture', projectId: 'p' }] });
            const report = await runDoctor(config, true);
            assert.equal(report.verification.model, 'passed');
            assert.equal(config.model.model, model, 'native model/context selection must not be rewritten');
        }
        assert.deepEqual(observed, ['fixture-model', 'fixture-model', 'fixture-model[custom]']);
    } finally {
        process.env = before; server.closeAllConnections(); await new Promise<void>(resolve => server.close(() => resolve()));
        await rm(dir, { recursive: true, force: true });
    }
});
