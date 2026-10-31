import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseConfig, modelHeaders } from '../src/config.ts';
import { nativeEnv } from '../src/runtime.ts';
import { Gateway, route } from '../src/gateway.ts';
import { prepareWorkspace } from '../src/workspace.ts';
import { fixture, message, rawConfig, until } from './helpers.ts';

test('native defaults require no Docker and use native edit permissions', () => {
    const config = parseConfig({ ...rawConfig('state'), runtime: undefined });
    assert.deepEqual(config.runtime, { maxWorkers: 8, idleMs: 900000 });
    assert.equal(config.projects[0]!.permissionMode, 'acceptEdits');
    assert.deepEqual(config.projects[0]!.allowedTools, []);
    assert.equal('maxConcurrent' in config.model, false);
    assert.throws(() => parseConfig({ ...rawConfig('state'), runtime: { mode: 'docker' } }), /Container runtime removed/);
    assert.throws(() => parseConfig({ ...rawConfig('state'), runtime: { image: 'old-image' } }), /unknown field/);
});

test('old native template stays usable without silently granting more permissions', () => {
    const config = parseConfig({ ...rawConfig('state'), projects: [{ id: 'p', maintainers: ['admin'], autoApproveWorkspace: false }] });
    assert.equal(config.projects[0]!.permissionMode, 'default');
    assert.throws(() => parseConfig({ ...rawConfig('state'), projects: [{ id: 'p', maintainers: ['admin'], autoApproveWorkspace: true }] }), /autoApproveWorkspace/);
    assert.throws(() => parseConfig({ ...rawConfig('state'), projects: [{ id: 'p', maintainers: ['admin'], permissionMode: 'bypassPermissions' }] }), /permissionMode/);
});

test('model headers resolve env references, fail closed on missing values and reject header injection', () => {
    const name = 'FEISHU_TEST_HEADER'; const prior = process.env[name];
    try {
        delete process.env[name];
        const c = parseConfig({ ...rawConfig('state'), model: { baseUrl: 'https://company.example/v1', model: 'd', extraHeaders: { 'x-team': 'env:' + name } } });
        assert.equal(c.model.baseUrl, 'https://company.example');
        assert.throws(() => modelHeaders(c), /Missing/);
        process.env[name] = 'project-a'; assert.deepEqual(modelHeaders(c), { 'x-team': 'project-a' });
        process.env[name] = 'a\r\nx-token:bad'; assert.throws(() => modelHeaders(c), /Invalid/);
        assert.throws(() => parseConfig({ ...rawConfig('state'), model: { baseUrl: 'https://company.example', model: 'd', extraHeaders: { 'bad\nname': 'x' } } }), /Invalid/);
    } finally { if (prior === undefined) delete process.env[name]; else process.env[name] = prior; }
});

test('native environment preserves enterprise networking without inheriting business credentials', () => {
    const before = { ...process.env };
    try {
        process.env.FEISHU_APP_SECRET = 'not-for-agent'; process.env.GITLAB_TOKEN = 'not-for-agent';
        process.env.HTTPS_PROXY = 'http://proxy.example:8080'; process.env.NODE_EXTRA_CA_CERTS = '/certs/company.pem';
        const env = nativeEnv({ root: '/room', work: '/room/work', home: '/room/home', inbound: '/room/inbound' });
        assert.equal(env.FEISHU_APP_SECRET, undefined); assert.equal(env.GITLAB_TOKEN, undefined);
        assert.equal(env.HTTPS_PROXY, process.env.HTTPS_PROXY); assert.equal(env.NODE_EXTRA_CA_CERTS, '/certs/company.pem');
        assert.equal(env.HOME, '/room/home'); assert.equal(env.USERPROFILE, '/room/home');
    } finally { process.env = before; }
});

function finish(w: Awaited<ReturnType<typeof fixture>>['workers'][number]): void {
    w.emit({ type: 'activity', background: 0, pinned: false, state: 'idle' });
    w.emit({ type: 'result', text: 'done', success: true, uuids: w.sent.map(d => d.uuid) });
}

test('idle service and background chatter start no CC; repeated normal messages reuse one process', async () => {
    const f = await fixture();
    try {
        await f.gateway.recover(); await f.gateway.sweep();
        await f.gateway.message(message({ mentioned: false }));
        assert.equal(f.workers.length, 0);
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        finish(f.workers[0]!);
        await f.gateway.message(message({ senderId: 'bob', text: 'continue' }));
        await until(() => f.workers[0]!.sent.length === 2);
        assert.equal(f.workers.length, 1);
        assert.equal(f.workers[0]!.sent[0]!.sessionId, f.workers[0]!.sent[1]!.sessionId);
    } finally { await f.close(); }
});

test('capacity pressure reclaims only an idle process and preserves its native session ID', async () => {
    const f = await fixture(); f.config.runtime.maxWorkers = 1; f.config.runtime.idleMs = 0;
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const nativeId = randomUUID(), old = f.workers[0]!, session = old.sent[0]!.sessionId;
        old.emit({ type: 'ready', ccId: nativeId }); finish(old);
        await f.gateway.message(message({ chatId: 'group2' })); await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(old.stopped, true);
        assert.equal(f.store.session(old.room.key).id, session); assert.equal(f.store.session(old.room.key).ccId, nativeId);
        finish(f.workers[1]!);
        await f.gateway.message(message()); await until(() => f.workers[2]?.sent.length === 1);
        assert.equal(f.workers[2]!.sent[0]!.sessionId, session);
    } finally { await f.close(); }
});

test('a waiting room starts immediately when another becomes idle, even with timed reaping disabled', async () => {
    const f = await fixture(); f.config.runtime.maxWorkers = 1; f.config.runtime.idleMs = 0;
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        await f.gateway.message(message({ chatId: 'group2' }));
        await until(() => f.transport.sent.some(s => s.text?.includes('当前会话都在工作')));
        assert.equal(f.workers.length, 1);
        finish(f.workers[0]!);
        await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers[1]!.room.chatId, 'group2');
    } finally { await f.close(); }
});

test('monitor pins, active subagents and unknown activity are never reclaimed for capacity', async () => {
    for (const background of [-1, 1, 0]) {
        const f = await fixture(); f.config.runtime.maxWorkers = 1; f.config.runtime.idleMs = 1;
        try {
            await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
            const w = f.workers[0]!;
            w.emit({ type: 'activity', background, pinned: background === 0 });
            w.emit({ type: 'result', text: 'watching', success: true, uuids: [w.sent[0]!.uuid] });
            await f.gateway.message(message({ chatId: 'group2' }));
            await f.gateway.sweep(Date.now() + 100000);
            assert.equal(w.stopped, false); assert.equal(f.workers.length, 1);
        } finally { await f.close(); }
    }
});

test('idle timeout releases process without resetting conversation', async () => {
    const f = await fixture(); f.config.runtime.idleMs = 1;
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, id = w.sent[0]!.sessionId;
        finish(w); await until(() => f.transport.sent.some(s => s.card));
        await new Promise(resolve => setTimeout(resolve, 10));
        await f.gateway.sweep(Date.now() + 5000);
        assert.equal(w.stopped, true); assert.equal(f.store.session(w.room.key).id, id);
        await f.gateway.message(message()); await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers[1]!.sent[0]!.sessionId, id);
    } finally { await f.close(); }
});

test('concurrent room admission never exceeds the live process limit', async () => {
    const f = await fixture(); f.config.runtime.maxWorkers = 1;
    try {
        await Promise.all([f.gateway.message(message()), f.gateway.message(message({ chatId: 'group2' })), f.gateway.message(message({ chatType: 'p2p', chatId: 'dm-a' }))]);
        await until(() => f.workers[0]?.sent.length === 1);
        assert.equal(f.workers.filter(w => !w.stopped).length, 1);
        finish(f.workers[0]!); await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers.filter(w => !w.stopped).length, 1);
        finish(f.workers[1]!); await until(() => f.workers[2]?.sent.length === 1);
        assert.equal(f.workers.filter(w => !w.stopped).length, 1);
    } finally { await f.close(); }
});

test('pending approvals prevent idle reclamation even after a background result', async () => {
    const f = await fixture(); f.config.runtime.maxWorkers = 1;
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'request', id: 'permit', kind: 'permission', name: 'Bash', input: { command: 'test' } });
        await until(() => f.gateway.approvals.size(w.sent[0]!.sessionId) === 1);
        finish(w);
        await f.gateway.message(message({ chatId: 'group2' })); await f.gateway.sweep(Date.now() + 10000000);
        assert.equal(w.stopped, false); assert.equal(f.workers.length, 1);
    } finally { await f.close(); }
});

test('gateway passes the prepared workspace once to the native factory', async () => {
    const f = await fixture(); await f.gateway.close();
    let observed = false;
    const g = new Gateway(f.config, f.store, f.transport, async (_r, _s, _e, workspace) => {
        observed = !!workspace?.work && !!workspace?.inbound;
        return { send: async () => {}, respond: () => {}, stop: async () => {} };
    });
    try { await g.message(message()); await until(() => observed); }
    finally { await g.close(); f.store.close(); await rm(f.dir, { recursive: true, force: true }); }
});

test('single UUID fallback clears completed input when result array is empty', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'result', text: 'done', success: true, uuid: w.sent[0]!.uuid, uuids: [] });
        assert.equal(f.store.countPending(w.room.key), 0);
    } finally { await f.close(); }
});

test('native workspace has no per-chat socket or Docker paths', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!, session = f.store.session(room.key);
        const w = await prepareWorkspace(f.config, room, session, f.config.projects[0]!);
        assert.deepEqual(Object.keys(w).sort(), ['home', 'inbound', 'root', 'work']);
        assert.equal('socketDir' in w, false);
    } finally { await f.close(); }
});
