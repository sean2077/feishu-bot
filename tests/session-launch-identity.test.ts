import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { fixture, message, until } from './helpers.ts';
import { route } from '../src/gateway.ts';
import { Store } from '../src/store.ts';
import type { Workspace } from '../src/types.ts';

test('launch identity persists across reopening and rejects cwd, configuration mode and config home changes', async () => {
    const f = await fixture();
    let reopened: Store | undefined;
    try {
        const session = f.store.session('owned-room');
        const workspace: Workspace = { root: f.dir, work: join(f.dir, 'project'), home: join(f.dir, 'home'), inbound: join(f.dir, 'inbound'), nativeConfig: 'inherit', configDir: join(f.dir, 'profile'), configDirOverride: false };
        f.store.bindLaunchIdentity(session, workspace, true);
        f.store.setCc(session.id, randomUUID());
        reopened = new Store(join(f.dir, 'state.sqlite'));
        const restored = reopened.session(session.roomKey);
        reopened.bindLaunchIdentity(restored, workspace, true);
        for (const change of [{ work: join(f.dir, 'other') }, { nativeConfig: 'isolated' as const }, { configDir: join(f.dir, 'other-profile') }, { configDirOverride: true }]) {
            assert.throws(() => reopened!.bindLaunchIdentity(restored, { ...workspace, ...change }, true), /启动身份与当前配置不匹配/);
            assert.equal(reopened.session(session.roomKey).ccId, restored.ccId);
            assert.equal(reopened.session(session.roomKey).id, session.id);
        }
        assert.throws(() => reopened!.bindLaunchIdentity({ ...restored, roomKey: 'other-room' }, workspace), /不属于当前聊天/);
        const fresh = reopened.newSession(session.roomKey);
        reopened.bindLaunchIdentity(fresh, { ...workspace, work: join(f.dir, 'other') }, true);
        assert.notEqual(fresh.id, restored.id);
        assert.equal(reopened.listSessions(session.roomKey).find(s => s.id === restored.id)?.ccId, restored.ccId);
    } finally { reopened?.close(); await f.close(); }
});

test('legacy isolated native sessions gain identity without allowing silent adoption into a host profile', async () => {
    const f = await fixture();
    try {
        const workspace: Workspace = { root: f.dir, work: join(f.dir, 'workspace'), home: join(f.dir, 'home'), inbound: join(f.dir, 'inbound') };
        const isolated = f.store.session('legacy-isolated');
        f.store.setCc(isolated.id, randomUUID());
        f.store.bindLaunchIdentity(isolated, workspace);
        const host = f.store.session('legacy-host');
        f.store.setCc(host.id, randomUUID());
        assert.throws(() => f.store.bindLaunchIdentity(host, { ...workspace, nativeConfig: 'inherit' }), /旧会话缺少/);
        assert.throws(() => f.store.bindLaunchIdentity(host, workspace, true), /旧会话缺少/);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM session_launch_identity WHERE session=?').get(host.id)?.n, 0);
    } finally { await f.close(); }
});

test('gateway refuses changed launch identity before starting a worker and retains the original session', async () => {
    const f = await fixture();
    try {
        const first = message(), room = route(f.config, first)!;
        await f.gateway.message(first);
        await until(() => f.workers[0]?.sent.length === 1);
        const session = f.store.session(room.key), ccId = randomUUID();
        f.workers[0]!.emit({ type: 'ready', ccId });
        await until(() => f.store.session(room.key).ccId === ccId);
        await f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        assert.equal(f.workers[0]!.stopped, true);
        const changed = join(f.dir, 'changed-project'); await mkdir(changed);
        f.config.projects[0]!.cwd = await realpath(changed);
        await f.gateway.message(message({ text: 'continue the same history' }));
        await until(() => !!f.store.db.prepare("SELECT 1 FROM audit WHERE event='dispatch_failed' AND detail LIKE '%启动身份%'").get());
        assert.equal(f.workers.length, 1, 'changed profile must fail before a native worker is started');
        assert.equal(f.store.session(room.key).id, session.id);
        assert.equal(f.store.session(room.key).ccId, ccId);
        assert.equal(f.store.countPending(room.key), 1, 'never-started input remains saved');
    } finally { await f.close(); }
});
