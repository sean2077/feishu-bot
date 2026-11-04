import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, realpath, rm } from 'node:fs/promises';
import { tmpdir, homedir } from 'node:os';
import { join } from 'node:path';
import { prepareWorkspace } from '../src/workspace.ts';
import { nativeEnv } from '../src/runtime.ts';
import { parseConfig } from '../src/config.ts';
import { rawConfig } from './helpers.ts';

test('direct cwd is not seeded or initialized and inherited config remains outside room state', async () => {
    const root = await mkdtemp(join(tmpdir(), 'feishu local runtime '));
    const prior = process.env.CLAUDE_CONFIG_DIR;
    try {
        const work = join(root, 'existing project'), configDir = join(root, 'user config');
        await mkdir(work); await mkdir(configDir);
        await writeFile(join(work, 'existing.txt'), 'unchanged');
        await writeFile(join(configDir, 'settings.json'), '{"fixture":true}');
        process.env.CLAUDE_CONFIG_DIR = configDir;
        const config = parseConfig({ ...rawConfig(join(root, 'state')), projects: [{ id: 'p', maintainers: ['admin'], cwd: work, nativeConfig: 'inherit', permissionMode: 'bypassPermissions' }] });
        const room = { key: 'fixture-room', chatId: 'fixture-chat', projectId: 'p', maintainers: ['admin'] };
        const session = { id: 'fixture-session', roomKey: room.key, state: 'open' as const, createdAt: 0 };
        const w = await prepareWorkspace(config, room, session, config.projects[0]!);
        assert.equal(w.work, await realpath(work));
        assert.equal(w.home, await realpath(homedir()));
        assert.equal(w.configDir, await realpath(configDir));
        assert.equal(w.nativeConfig, 'inherit');
        assert.equal(w.configDirOverride, true);
        assert.equal(w.inbound, join(w.root, 'inbound'));
        assert.equal(await readFile(join(work, 'existing.txt'), 'utf8'), 'unchanged');
        assert.equal(await readFile(join(configDir, 'settings.json'), 'utf8'), '{"fixture":true}');
        await assert.rejects(readFile(join(work, '.git', 'config')), { code: 'ENOENT' });
        const missing = { ...config.projects[0]!, cwd: join(root, 'does-not-exist') };
        await assert.rejects(prepareWorkspace(config, room, session, missing), { code: 'ENOENT' });
        process.env.CLAUDE_CONFIG_DIR = join(root, 'missing-config');
        await assert.rejects(prepareWorkspace(config, room, session, config.projects[0]!), { code: 'ENOENT' });
    } finally {
        if (prior === undefined) delete process.env.CLAUDE_CONFIG_DIR; else process.env.CLAUDE_CONFIG_DIR = prior;
        await rm(root, { recursive: true, force: true });
    }
});

test('inherited native environment retains tool credentials but removes bridge and parent session controls', () => {
    const before = { ...process.env };
    try {
        Object.assign(process.env, { FIXTURE_MCP_TOKEN: 'fixture-mcp', FIXTURE_FEISHU_SECRET: 'fixture-feishu', FEISHU_ACTION_TOKEN: 'fixture-action', FEISHU_ACTION_URL: 'fixture-url', CLAUDECODE: '1', CLAUDE_CODE_SESSION_ID: 'fixture-parent', CLAUDE_CODE_SSE_PORT: '1234', CLAUDE_CODE_ENTRYPOINT: 'cli', CLAUDE_AGENT_SDK_SESSION_ID: 'fixture-sdk' });
        process.env.CLAUDE_CONFIG_DIR = '/user/custom-config';
        const w = { root: '/room', work: '/project', home: '/user', inbound: '/room/inbound', nativeConfig: 'inherit' as const, configDir: '/user/custom-config' };
        const env = nativeEnv(w, 'FIXTURE_FEISHU_SECRET');
        assert.equal(env.FIXTURE_MCP_TOKEN, 'fixture-mcp');
        for (const key of ['FIXTURE_FEISHU_SECRET', 'FEISHU_ACTION_TOKEN', 'FEISHU_ACTION_URL', 'CLAUDECODE', 'CLAUDE_CODE_SESSION_ID', 'CLAUDE_CODE_SSE_PORT', 'CLAUDE_CODE_ENTRYPOINT', 'CLAUDE_AGENT_SDK_SESSION_ID']) assert.equal(env[key], undefined, key);
        assert.equal(env.HOME, '/user'); assert.equal(env.USERPROFILE, '/user'); assert.equal(env.CLAUDE_CONFIG_DIR, '/user/custom-config');
        delete process.env.CLAUDE_CONFIG_DIR;
        assert.equal(nativeEnv(w, 'FIXTURE_FEISHU_SECRET').CLAUDE_CONFIG_DIR, undefined, 'unset override must retain native global .claude.json location');
        assert.equal(process.env.FIXTURE_FEISHU_SECRET, 'fixture-feishu', 'parent environment must remain unchanged');
    } finally { process.env = before; }
});
