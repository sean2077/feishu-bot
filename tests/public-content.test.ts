import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, readdir, access, mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';
import { parseConfig } from '../src/config.ts';
import { nativeEnv } from '../src/runtime.ts';

const root = fileURLToPath(new URL('../', import.meta.url));
const example = async () => JSON.parse(await readFile(join(root, 'examples/config.example.json'), 'utf8'));

test('the example uses fictional identifiers, a reserved domain and explicit secret references', async () => {
    const raw = await example();
    const config = parseConfig(raw, root);
    assert.equal(new URL(config.model.baseUrl).hostname, 'model.example');
    assert.equal(config.model.tokenEnv, 'MODEL_AUTH_TOKEN');
    assert.equal(config.appSecretEnv, 'FEISHU_APP_SECRET');
    assert.ok(config.appId.startsWith('cli_example_'));
    assert.ok(config.groups.every(g => g.chatId.startsWith('oc_example_')));
    assert.ok(config.dms.every(d => d.userId.startsWith('ou_example_')));
    assert.equal(config.projects[0]!.seedDir, undefined);
    assert.deepEqual(config.projects[0]!.actions, []);
    assert.equal('apiKey' in raw.model, false);
    assert.equal('appSecret' in raw, false);
});

test('personal usage requires no group, source repository or external action', async () => {
    const raw = await example();
    raw.groups = [];
    raw.projects[0].permissionMode = 'default';
    const config = parseConfig(raw, root);
    assert.deepEqual(config.groups, []);
    assert.equal(config.dms.length, 1);
    assert.equal(config.projects[0]!.permissionMode, 'default');
    assert.equal(config.projects[0]!.seedDir, undefined);
    assert.deepEqual(config.projects[0]!.actions, []);
});

test('explicit credential variable names remain supported without silent fallback', async () => {
    const raw = await example();
    raw.model.tokenEnv = 'CUSTOM_MODEL_SECRET';
    assert.equal(parseConfig(raw).model.tokenEnv, 'CUSTOM_MODEL_SECRET');
    delete raw.model.tokenEnv;
    assert.equal(parseConfig(raw).model.tokenEnv, 'MODEL_AUTH_TOKEN');
    assert.equal(parseConfig(raw).projects[0]!.permissionMode, 'acceptEdits');
});

test('native environment inherits an explicit timezone and does not invent one', () => {
    const previous = process.env.TZ;
    const workspace = { root: '/room', work: '/room/work', home: '/room/home', inbound: '/room/inbound' };
    try {
        for (const zone of ['UTC', 'Europe/Paris']) {
            process.env.TZ = zone;
            assert.equal(nativeEnv(workspace).TZ, zone);
        }
        delete process.env.TZ;
        assert.equal(nativeEnv(workspace).TZ, undefined);
    } finally {
        if (previous === undefined) delete process.env.TZ; else process.env.TZ = previous;
    }
});

test('usage snippets merge into a valid main configuration', async () => {
    const text = await readFile(join(root, 'docs/usage.md'), 'utf8');
    const snippets = [...text.matchAll(/```json\s*\n([\s\S]*?)```/g)];
    assert.ok(snippets.length > 0);
    for (const match of snippets) parseConfig({ ...await example(), ...JSON.parse(match[1]!) }, root);
});

test('public documentation has no broken local file links', async () => {
    const docs = ['README.md', 'SECURITY.md', 'CONTRIBUTING.md', 'AGENTS.md',
        ...(await readdir(join(root, 'docs'))).filter(f => f.endsWith('.md')).map(f => `docs/${f}`)];
    for (const name of docs) {
        const text = await readFile(join(root, name), 'utf8');
        for (const match of text.matchAll(/\[[^\]]*\]\(([^\s)]+)\)/g)) {
            const link = match[1]!;
            if (/^[a-z][a-z0-9+.-]*:/i.test(link) || link.startsWith('#')) continue;
            const path = decodeURIComponent(link.split('#')[0]!);
            await assert.doesNotReject(access(resolve(root, dirname(name), path)), `${name}: missing ${path}`);
        }
    }
});

test('local configuration and data files are ignored while distributable examples remain visible', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-ignore-'));
    try {
        await writeFile(join(dir, '.gitignore'), await readFile(join(root, '.gitignore')));
        execFileSync('git', ['init', '-q'], { cwd: dir });
        const paths = ['config.json', 'config.local.json', '.env.local', 'state/history.jsonl', 'diagnostic.sqlite', 'credential.key',
            'examples/config.example.json', 'docs/usage.md'];
        const ignored = new Set(execFileSync('git', ['check-ignore', '--no-index', '--stdin'], {
            cwd: dir, input: paths.join('\n') + '\n', encoding: 'utf8'
        }).trim().split('\n'));
        for (const path of paths.slice(0, 6)) assert.ok(ignored.has(path), path);
        for (const path of paths.slice(6)) assert.ok(!ignored.has(path), path);
    } finally { await rm(dir, { recursive: true, force: true }); }
});
