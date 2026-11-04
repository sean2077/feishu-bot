import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseConfig } from '../src/config.ts';
import { runDoctor } from '../src/doctor.ts';

async function fixture(t: import('node:test').TestContext) {
    const dir = await mkdtemp(join(tmpdir(), 'local-doctor-'));
    const before = { ...process.env };
    process.env.LOCAL_DOCTOR_APP = 'fixture-app'; process.env.LOCAL_DOCTOR_MODEL = 'fixture-model';
    t.after(async () => { process.env = before; await rm(dir, { recursive: true, force: true }); });
    t.mock.method(globalThis, 'fetch', async () => { throw new Error('No external requests allowed'); });
    return { dir, raw: { tenantId: 'tenant_fixture', appId: 'cli_fixture', appSecretEnv: 'LOCAL_DOCTOR_APP', dataDir: join(dir, 'state'),
        model: { baseUrl: 'https://endpoint.test', tokenEnv: 'LOCAL_DOCTOR_MODEL', model: 'fixture-model' },
        projects: [{ id: 'assistant', maintainers: ['ou_fixture'] }], dms: [{ userId: 'ou_fixture', projectId: 'assistant' }] } };
}

test('doctor checks real cwd and inherited config directories without displaying private paths', async t => {
    const { dir, raw } = await fixture(t);
    const cwd = join(dir, 'private-work'), configDir = join(dir, 'private-config');
    await mkdir(cwd); await mkdir(configDir);
    process.env.CLAUDE_CONFIG_DIR = configDir;
    const config = parseConfig({ ...raw, projects: [{ ...raw.projects[0], cwd, nativeConfig: 'inherit' }] });
    const report = await runDoctor(config);
    assert.equal(report.ready, true);
    assert.equal(report.checks.find(c => c.name === 'assistant cwd')!.ok, true);
    assert.match(report.checks.find(c => c.name === 'assistant inherited native configuration')!.detail!, /CLAUDE_CONFIG_DIR/);
    assert.equal(JSON.stringify(report).includes(dir), false);
    await rm(configDir, { recursive: true }); await writeFile(cwd + '-file', 'fixture');
    config.projects[0]!.cwd = cwd + '-file';
    const failed = await runDoctor(config);
    assert.equal(failed.checks.find(c => c.name === 'assistant cwd')!.ok, false);
    assert.equal(failed.checks.find(c => c.name === 'assistant inherited native configuration')!.ok, false);
    assert.equal(JSON.stringify(failed).includes(dir), false);
});

test('isolated doctor ignores a missing host config and rejects missing or non-Claude executables', async t => {
    const { dir, raw } = await fixture(t);
    process.env.CLAUDE_CONFIG_DIR = join(dir, 'missing-private-config');
    const normal = await runDoctor(parseConfig(raw));
    assert.equal(normal.ready, true);
    assert.equal(normal.checks.some(c => c.name.includes('inherited native')), false);
    for (const executable of [join(dir, 'missing-private-executable'), dir, process.execPath]) {
        const failed = await runDoctor(parseConfig({ ...raw, runtime: { claudeExecutable: executable } }));
        assert.equal(failed.checks.find(c => c.name === 'configured Claude Code')!.ok, false);
        assert.equal(JSON.stringify(failed).includes(executable), false);
    }
});

test('doctor validates an executable version response rather than its filename', { skip: process.platform === 'win32' }, async t => {
    const { dir, raw } = await fixture(t);
    const executable = join(dir, 'fixture-cli');
    await writeFile(executable, '#!/bin/sh\nprintf "1.2.3 (Claude Code)\\n"\n', { mode: 0o700 });
    const report = await runDoctor(parseConfig({ ...raw, runtime: { claudeExecutable: executable } }));
    assert.equal(report.checks.find(c => c.name === 'configured Claude Code')!.ok, true);
    assert.equal(JSON.stringify(report).includes(executable), false);
});
