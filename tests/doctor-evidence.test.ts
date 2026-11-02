import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseConfig } from '../src/config.ts';
import { parseCli } from '../src/cli-options.ts';
import { runDoctor } from '../src/doctor.ts';

async function fixture(t: import('node:test').TestContext) {
    const dir = await mkdtemp(join(tmpdir(), 'doctor-evidence-'));
    const before = { ...process.env };
    process.env.EVIDENCE_APP_SECRET = 'fixture-app-credential';
    process.env.EVIDENCE_MODEL_TOKEN = 'fixture-model-credential';
    t.after(async () => { process.env = before; await rm(dir, { recursive: true, force: true }); });
    return parseConfig({ tenantId: 'tenant_fixture', appId: 'cli_fixture', dataDir: dir,
        appSecretEnv: 'EVIDENCE_APP_SECRET',
        model: { baseUrl: 'https://model.fixture.test', tokenEnv: 'EVIDENCE_MODEL_TOKEN', model: 'fixture-model' },
        projects: [{ id: 'assistant', maintainers: ['ou_fixture'] }],
        dms: [{ userId: 'ou_fixture', projectId: 'assistant' }] });
}

test('doctor distinguishes local prerequisites from unverified live capabilities without networking', async t => {
    const config = await fixture(t);
    const fetch = t.mock.method(globalThis, 'fetch', async () => { throw new Error('must not call network'); });
    const report = await runDoctor(config);
    assert.equal(report.ready, true);
    assert.deepEqual(report.verification, {
        local: 'passed', model: 'not_checked', feishuAuth: 'not_checked', feishuIdentity: 'not_checked',
        messageDelivery: 'not_checked', cardCallbacks: 'not_checked', fileDelivery: 'not_checked', nativeTools: 'not_checked'
    });
    assert.ok(report.nextSteps.some(step => step.includes('--probe-feishu')));
    assert.equal(fetch.mock.callCount(), 0);
});

test('Feishu identity probe is a separate explicit doctor-only opt-in', () => {
    const options = parseCli(['doctor', '--probe-feishu']);
    assert.equal(options.probeFeishu, true);
    assert.equal(options.probe, false, 'checking app identity must not bill a model request');
    assert.throws(() => parseCli(['run', '--probe-feishu']));
    assert.throws(() => parseCli(['doctor', '--probe-feishu', '--probe-feishu']));
});

test('identity success does not claim message, callback, file or model acceptance', async t => {
    const config = await fixture(t);
    const calls: { url: string; init?: RequestInit }[] = [];
    t.mock.method(globalThis, 'fetch', async (input: string | URL | Request, init?: RequestInit) => {
        calls.push({ url: String(input), init });
        return Response.json(calls.length === 1 ? { code: 0, tenant_access_token: 'fixture-access-token' }
            : { code: 0, bot: { open_id: 'ou_fixture_bot', app_name: 'private-fixture-name' } });
    });
    const report = await runDoctor(config, false, true);
    assert.equal(report.verification.feishuAuth, 'passed');
    assert.equal(report.verification.feishuIdentity, 'passed');
    for (const key of ['model', 'messageDelivery', 'cardCallbacks', 'fileDelivery', 'nativeTools'] as const)
        assert.equal(report.verification[key], 'not_checked');
    assert.deepEqual(calls.map(c => c.url), [
        'https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal',
        'https://open.feishu.cn/open-apis/bot/v3/info'
    ]);
    assert.equal(calls[0]!.init?.method, 'POST');
    assert.equal(calls[1]!.init?.method, 'GET');
    assert.ok(calls.every(c => c.init?.redirect === 'manual' && c.init?.signal));
    const output = JSON.stringify(report);
    for (const value of ['fixture-app-credential', 'fixture-model-credential', 'fixture-access-token', 'private-fixture-name', 'ou_fixture_bot'])
        assert.equal(output.includes(value), false);
});

test('failed identity has a safe stage-specific error and never sends a model request', async t => {
    const config = await fixture(t);
    let calls = 0;
    t.mock.method(globalThis, 'fetch', async () => {
        calls++;
        return Response.json(calls === 1 ? { code: 0, tenant_access_token: 'fixture-access-token' }
            : { code: 99991672, msg: 'private-provider-detail fixture-access-token' });
    });
    const report = await runDoctor(config, false, true);
    assert.equal(report.ready, false);
    assert.equal(report.verification.feishuAuth, 'passed');
    assert.equal(report.verification.feishuIdentity, 'failed');
    assert.equal(calls, 2);
    assert.match(JSON.stringify(report), /99991672/);
    assert.doesNotMatch(JSON.stringify(report), /private-provider-detail|fixture-access-token/);
});

test('invalid Feishu credentials block the probe before any request', async t => {
    const config = await fixture(t);
    process.env.EVIDENCE_APP_SECRET = 'replace-with-app-secret';
    const fetch = t.mock.method(globalThis, 'fetch', async () => { throw new Error('must not call network'); });
    const report = await runDoctor(config, false, true);
    assert.equal(report.ready, false);
    assert.equal(report.verification.feishuAuth, 'blocked');
    assert.equal(report.verification.feishuIdentity, 'blocked');
    assert.equal(fetch.mock.callCount(), 0);
});
