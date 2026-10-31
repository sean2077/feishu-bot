import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { route } from '../src/gateway.ts';
import { fixture, message, until } from './helpers.ts';
test('same-group participants share a session, other groups and DMs do not', async () => {
    const f = await fixture();
    try {
        const a = route(f.config, message())!, b = route(f.config, message({ senderId: 'bob' }))!;
        assert.equal(a.key, b.key);
        assert.notEqual(a.key, route(f.config, message({ chatId: 'group2' }))!.key);
        assert.notEqual(route(f.config, message({ chatId: 'dm', chatType: 'p2p' }))!.key, route(f.config, message({ chatId: 'dm', chatType: 'p2p', senderId: 'bob' }))!.key);
        assert.equal(route(f.config, message({ chatId: 'unlisted' })), undefined);
        assert.equal(route(f.config, message({ senderIsBot: true })), undefined);
        assert.equal(route(f.config, message({ chatType: 'p2p', senderId: 'stranger' })), undefined);
    }
    finally {
        await f.close();
    }
});
test('unmentioned context does not wake CC; authors and incremental context are preserved', async () => {
    const f = await fixture();
    try {
        f.config.limits.backgroundMessages = 30; // Explicit opt-in only.
        const background = message({ senderId: 'bob', text: 'Only Windows fails', mentioned: false });
        await f.gateway.message(background);
        assert.equal(f.workers.length, 0);
        await f.gateway.message(message({ text: 'Investigate' }));
        await until(() => f.workers[0]?.sent.length === 1);
        assert.equal(f.workers[0]!.sent[0]!.background[0]!.senderId, 'bob');
        assert.equal(f.workers[0]!.sent[0]!.message.senderId, 'alice');
        await f.gateway.message(message({ text: 'Another detail' }));
        await until(() => f.workers[0]!.sent.length === 2);
        assert.deepEqual(f.workers[0]!.sent[1]!.background, []);
    }
    finally {
        await f.close();
    }
});
test('messages enter the active runtime without waiting for a result', async () => {
    const f = await fixture();
    try {
        const first = message();
        await f.gateway.message(first);
        await until(() => f.workers[0]?.sent.length === 1);
        await f.gateway.message(message({ senderId: 'bob', text: 'use the other branch' }));
        await until(() => f.workers[0]!.sent.length === 2);
        assert.equal(f.workers.length, 1);
        const w = f.workers[0]!;
        for (const d of w.sent)
            w.emit({ type: 'consumed', uuid: d.uuid });
        w.emit({ type: 'result', text: 'Combined answer', success: true, uuid: w.sent[1]!.uuid });
        await until(() => f.transport.sent.some(s => s.card !== undefined));
        await f.gateway.message(message({ text: 'next request' }));
        await until(() => w.sent.length === 3);
        assert.equal(f.workers.length, 1);
    }
    finally {
        await f.close();
    }
});
test('two groups and two DMs have independent workers and writable workspaces', async () => {
    const f = await fixture();
    try {
        for (const m of [message(), message({ chatId: 'group2' }), message({ chatId: 'dm-a', chatType: 'p2p' }), message({ chatId: 'dm-b', chatType: 'p2p', senderId: 'bob' })])
            await f.gateway.message(m);
        await until(() => f.workers.length === 4 && f.workers.every(w => w.sent.length === 1));
        assert.equal(new Set(f.workers.map(w => w.sent[0]!.sessionId)).size, 4);
        assert.equal(new Set(f.workers.map(w => w.room.key)).size, 4);
    }
    finally {
        await f.close();
    }
});
test('deduplication survives changed text and never re-executes a control command', async () => {
    const f = await fixture();
    try {
        const m = message();
        await f.gateway.message(m);
        await f.gateway.message({ ...m, text: 'changed' });
        await until(() => f.workers[0]?.sent.length === 1);
        assert.equal(f.workers[0]!.sent[0]!.message.text, 'test');
        const reset = message({ senderId: 'admin', text: '/new' });
        await f.gateway.message(reset);
        const room = route(f.config, m)!;
        const id = f.store.session(room.key).id;
        await f.gateway.message(reset);
        assert.equal(f.store.session(room.key).id, id);
    }
    finally {
        await f.close();
    }
});
test('quoted content is scoped to current chat and arbitrary replies do not trigger', async () => {
    const f = await fixture();
    try {
        f.transport.quoted.set('foreign', message({ chatId: 'group2', text: 'secret' }));
        await f.gateway.message(message({ replyTo: 'foreign' }));
        await until(() => f.workers[0]?.sent.length === 1);
        assert.equal(f.workers[0]!.sent[0]!.quote, undefined);
        await f.gateway.message(message({ replyTo: 'random', mentioned: false }));
        assert.equal(f.workers[0]!.sent.length, 1);
        const room = f.workers[0]!.room;
        f.store.recordOutput('our-reply', room.key, f.workers[0]!.sent[0]!.sessionId);
        await f.gateway.message(message({ replyTo: 'our-reply', mentioned: false }));
        assert.equal(f.workers[0]!.sent.length, 1);
        await f.gateway.message(message({ replyTo: 'our-reply', mentioned: true }));
        await until(() => f.workers[0]!.sent.length === 2);
    }
    finally {
        await f.close();
    }
});
test('permission wait does not block new input; only a maintainer can decide', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'request', kind: 'permission', id: 'tool-1', name: 'Bash', input: { command: 'echo hello' } });
        await until(() => !!f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get());
        const id = f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id as string;
        await f.gateway.message(message({ senderId: 'bob', text: 'extra detail' }));
        await until(() => w.sent.length === 2);
        assert.match(await f.gateway.approvals.click('group1', 'bob', { request: id, decision: 'allow' }), /维护者/);
        assert.equal(await f.gateway.approvals.click('group2', 'admin', { request: id, decision: 'allow' }), '');
        assert.equal(w.responses.length, 0);
        assert.equal(await f.gateway.approvals.click('group1', 'admin', { request: id, decision: 'allow' }), '已批准一次');
        await until(() => w.responses.length === 1);
        assert.equal(w.responses[0]!.value.approved, true);
        assert.match(await f.gateway.approvals.click('group1', 'admin', { request: id, decision: 'allow' }), /过期|结束/);
    }
    finally {
        await f.close();
    }
});
test('stop cancels approvals and queued inputs; old worker output is ignored', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'request', kind: 'permission', id: 'op', name: 'Bash', input: { command: 'test' } });
        await until(() => !!f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get());
        const oldSession = w.sent[0]!.sessionId;
        await f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        assert.equal(w.stopped, true);
        assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM requests WHERE state='pending'").get()!.n, 0);
        const n = f.transport.sent.length;
        w.emit({ type: 'result', text: 'obsolete answer', success: true });
        await new Promise(r => setTimeout(r, 20));
        assert.equal(f.transport.sent.length, n);
        await f.gateway.message(message({ text: 'start new' }));
        await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers[1]!.sent[0]!.sessionId, oldSession);
    }
    finally {
        await f.close();
    }
});
test('a foreign session ID cannot interrupt or replace the current conversation', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await f.gateway.message(message({ chatId: 'group2' }));
        await until(() => f.workers.length === 2 && f.workers.every(w => w.sent.length));
        const first = f.workers.find(w => w.room.chatId === 'group1')!, second = f.workers.find(w => w.room.chatId === 'group2')!;
        await f.gateway.message(message({ senderId: 'admin', text: '/resume ' + second.sent[0]!.sessionId }));
        assert.equal(first.stopped, false);
        assert.match(f.transport.sent.at(-1)!.text!, /本聊天/);
    }
    finally {
        await f.close();
    }
});
test('recover marks ambiguous deliveries and operations uncertain without replaying them', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const old = f.workers[0]!.sent[0]!;
        const recovery = f.store.recover();
        assert.deepEqual(recovery.rooms, [old.roomKey]);
        assert.equal(f.store.pending(old.roomKey).length, 0);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE uuid=?').get(old.uuid)!.status, 'uncertain');
    }
    finally {
        await f.close();
    }
});
test('background cache has a strict per-room bound', async () => {
    const f = await fixture();
    try {
        f.config.limits.backgroundMessages = 3;
        for (let i = 0; i < 20; i++)
            await f.gateway.message(message({ mentioned: false, text: String(i), time: Date.now() + i }));
        assert.equal(f.workers.length, 0);
        const room = route(f.config, message())!;
        assert.equal(f.store.background(room.key, Date.now() + 100, 10000, 100).length, 3);
    }
    finally {
        await f.close();
    }
});
test('native monitor pin prevents idle process reclamation', async () => {
    const f = await fixture();
    try {
        f.config.runtime.idleMs = 1;
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, d = w.sent[0]!;
        w.emit({ type: 'consumed', uuid: d.uuid });
        w.emit({ type: 'activity', tool: 'Monitor' });
        w.emit({ type: 'result', text: 'Watching', success: true, uuid: d.uuid });
        await f.gateway.sweep(Date.now() + 100000);
        assert.equal(w.stopped, false);
    }
    finally {
        await f.close();
    }
});
test('merged native result settles every attributed input, not the whole session', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await f.gateway.message(message({ senderId: 'bob' }));
        await until(() => f.workers[0]?.sent.length === 2);
        const w = f.workers[0], ids = w.sent.map(d => d.uuid);
        w.emit({ type: 'result', text: 'combined', success: true, uuid: ids[1], uuids: ids });
        assert.equal(f.store.countPending(w.room.key), 0);
        await f.gateway.message(message());
        await until(() => w.sent.length === 3);
        w.emit({ type: 'result', text: 'background result', success: true });
        assert.equal(f.store.countPending(w.room.key), 1);
    }
    finally {
        await f.close();
    }
});
test('private member operation reaches maintainer DM without routing private history', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message({ chatType: 'p2p', chatId: 'dm-alice', text: 'private context' }));
        await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0];
        w.emit({ type: 'request', kind: 'permission', id: 'private-op', name: 'Write', input: { file_path: 'report.txt' } });
        await until(() => f.transport.sent.some(s => s.room.chatId === 'admin' && s.card));
        const card = f.transport.sent.find(s => s.room.chatId === 'admin' && s.card);
        const id = f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get().id;
        assert.ok(!JSON.stringify(card.card).includes('private context'));
        assert.equal(await f.gateway.approvals.click('admin-dm', 'admin', { request: id, decision: 'allow' }, 'copied-card'), '');
        assert.equal(await f.gateway.approvals.click('dm-alice', 'alice', { request: id, decision: 'allow' }, card.id), '');
        assert.equal(await f.gateway.approvals.click('admin-dm', 'admin', { request: id, decision: 'allow' }, card.id), '已批准一次');
        await until(() => w.responses.length === 1);
        assert.equal(w.responses[0].value.approved, true);
    }
    finally {
        await f.close();
    }
});
