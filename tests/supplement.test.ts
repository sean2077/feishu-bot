import test from 'node:test';
import assert from 'node:assert/strict';
import { Gateway } from '../src/gateway.ts';
import { route } from '../src/policy.ts';
import { fixture, message, until } from './helpers.ts';
import type { Delivery, Message } from '../src/types.ts';

type Fixture = Awaited<ReturnType<typeof fixture>>;
const state = (f: Fixture, input: Message) => f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status;
const reply = (f: Fixture, input: Message) => f.transport.sent.find(s => s.replyTo === input.id)?.text ?? '';
async function start(f: Fixture) {
    const input = message({ text: 'original task' });
    await f.gateway.message(input);
    await until(() => f.workers[0]?.sent.length === 1);
    return { input, worker: f.workers[0]!, delivery: f.workers[0]!.sent[0]! };
}
function finish(worker: Fixture['workers'][number], deliveries: Delivery[] = worker.sent) {
    worker.emit({ type: 'activity', background: 0 });
    worker.emit({ type: 'result', text: 'finished', success: true, uuids: deliveries.map(d => d.uuid) });
}

test('btw and ps require a running foreground task and never create idle sessions', async () => {
    const f = await fixture();
    try {
        for (const text of ['/btw', '/ps   ', '/btw extra context', '/ps /new']) {
            const input = message({ text });
            await f.gateway.message(input);
            assert.match(reply(f, input), text.trim().includes(' ') ? /没有正在执行的前台任务/ : /用法/);
        }
        assert.equal(f.workers.length, 0);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM sessions').get()!.n, 0);
        assert.equal(f.transport.receipts.length, 0);
        const help = message({ text: '/help' }); await f.gateway.message(help);
        assert.match(reply(f, help), /\/btw/); assert.match(reply(f, help), /\/ps/);
    } finally { await f.close(); }
});

test('supplement authorization runs before receipts, quote expansion, downloads and storage', async () => {
    const f = await fixture();
    let downloads = 0, fetches = 0;
    f.transport.download = async () => { downloads++; return Buffer.from('fixture'); };
    f.transport.fetch = async () => { fetches++; return undefined; };
    try {
        for (const overrides of [{ senderId: 'stranger' }, { chatId: 'foreign' }, { mentioned: false }, { chatType: 'p2p' as const, senderId: 'stranger' }])
            await f.gateway.message(message({ text: '/btw private input', resources: [{ key: 'fixture', kind: 'file', name: 'fixture.txt' }], replyTo: 'private-quote', ...overrides }));
        assert.equal(f.transport.sent.length, 0); assert.equal(f.transport.receipts.length, 0);
        assert.equal(downloads, 0); assert.equal(fetches, 0); assert.equal(f.workers.length, 0);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n, 0);
    } finally { await f.close(); }
});

test('supplement bodies, authors, UUIDs, quotes and attachments use the existing stream and one answer card', async () => {
    const f = await fixture();
    try {
        const { input, worker, delivery } = await start(f);
        const quote = message({ text: 'quoted fact', mentioned: false }); f.transport.quoted.set(quote.id, quote);
        const first = message({ senderId: 'bob', text: '/btw /new', replyTo: quote.id, threadId: 'fixture-thread', resources: [{ key: 'file-key', kind: 'file', name: 'fact.txt' }] });
        const second = message({ senderId: 'alice', text: '/ps /compact keep this as text' });
        await f.gateway.message(first); await f.gateway.message(first); await f.gateway.message(second);
        await until(() => worker.sent.length === 3);
        assert.equal(worker.sent[1]!.message.text, '/new');
        assert.equal(worker.sent[2]!.message.text, '/compact keep this as text');
        assert.equal(worker.sent[1]!.message.id, first.id); assert.equal(worker.sent[1]!.message.senderId, 'bob');
        assert.equal(worker.sent[1]!.message.threadId, 'fixture-thread');
        assert.equal(worker.sent[1]!.quote?.id, quote.id); assert.equal(worker.sent[1]!.attachments.length, 1);
        assert.equal(worker.sent[1]!.command, undefined); assert.equal(worker.sent[2]!.command, undefined);
        assert.equal(new Set(worker.sent.map(d => d.uuid)).size, 3); assert.equal(f.workers.length, 1);
        assert.ok(worker.sent.every(d => d.sessionId === delivery.sessionId));
        await until(() => /已交给原生输入流/.test(reply(f, first)));
        assert.match(reply(f, first), /不代表模型已读取/);
        finish(worker);
        await until(() => [input, first, second].every(m => f.transport.receiptStates.some(r => r.id === m.id && r.state === 'done')));
        assert.equal(f.transport.sent.filter(s => s.card).length, 1, 'supplements must not open separate answer cards');
        assert.equal(f.transport.sent.find(s => s.card)!.replyTo, input.id);
        assert.equal(f.store.listSessions(worker.room.key).length, 1);
    } finally { await f.close(); }
});

test('idle warm workers and background-only activity are not supplement targets', async () => {
    const f = await fixture();
    try {
        const { worker, input } = await start(f); finish(worker);
        await until(() => state(f, input) === 'done');
        for (const background of [0, 2, -1]) {
            worker.emit({ type: 'activity', state: background === 0 ? 'idle' : 'running', background, pinned: background !== 0 });
            const extra = message({ text: '/ps do not start another task' }); await f.gateway.message(extra);
            assert.match(reply(f, extra), /没有正在执行的前台任务/);
            assert.equal(worker.sent.length, 1); assert.equal(f.workers.length, 1);
        }
    } finally { await f.close(); }
});

test('a pending compact is a barrier to supplements, not to approval callbacks', async () => {
    const f = await fixture();
    try {
        const { worker, delivery } = await start(f);
        worker.emit({ type: 'request', id: 'native-permission', kind: 'permission', name: 'Write', input: { file_path: 'fixture-only' } });
        await until(() => f.gateway.approvals.size(worker.session.id) === 1 && f.transport.sent.some(s => s.card));
        const request = String(f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id);
        const card = f.transport.sent.find(s => s.card)!;
        const compact = message({ senderId: 'admin', text: '/compact' }); await f.gateway.message(compact);
        const extra = message({ senderId: 'bob', text: '/btw later detail' }); await f.gateway.message(extra);
        assert.equal(state(f, extra), 'queued'); assert.match(reply(f, extra), /等待.*compact/);
        assert.equal(worker.sent.length, 1);
        assert.match(await f.gateway.approvals.click(worker.room.chatId, 'bob', { request, decision: 'allow' }, card.id), /维护者/);
        assert.match(await f.gateway.approvals.click(worker.room.chatId, 'admin', { request, decision: 'allow' }, card.id), /已批准/);
        await until(() => worker.responses.some(r => r.id === 'native-permission' && r.value.approved === true));
        finish(worker, [delivery]);
        await until(() => state(f, extra) === 'cancelled' && worker.sent.length === 2);
        assert.equal(worker.sent[1]!.command?.name, 'compact');
        assert.equal(worker.sent.some(d => d.message.id === extra.id), false);
        assert.ok(!f.transport.receiptStates.some(r => r.id === extra.id && r.state === 'done'));
        await until(() => f.transport.sent.some(s => s.replyTo === extra.id && JSON.stringify(s.card ?? s.text).includes('补充已取消')));
    } finally { await f.close(); }
});

test('a foreground result during supplement download cancels the unsent input instead of starting a new task', async () => {
    const f = await fixture();
    let release!: (value: Uint8Array) => void, downloading = false;
    f.transport.download = async () => { downloading = true; return new Promise<Uint8Array>(resolve => { release = resolve; }); };
    try {
        const { worker, delivery } = await start(f);
        const extra = message({ text: '/btw attachment detail', resources: [{ key: 'image-key', kind: 'image', name: 'image.png' }] });
        await f.gateway.message(extra); await until(() => downloading);
        finish(worker, [delivery]); await until(() => state(f, extra) === 'cancelled');
        release(Buffer.from('fixture image'));
        await f.gateway.message(message({ text: '/status' }));
        await new Promise(resolve => setTimeout(resolve, 30));
        assert.equal(worker.sent.length, 1); assert.equal(state(f, extra), 'cancelled');
        assert.ok(!f.transport.receiptStates.some(r => r.id === extra.id && r.state === 'done'));
    } finally { release?.(Buffer.from('cleanup')); await f.close(); }
});

for (const command of ['/stop', '/new']) test(`${command} wins a pending supplement download and late completion cannot revive it`, async () => {
    const f = await fixture();
    let release!: (value: Uint8Array) => void, downloading = false;
    f.transport.download = async () => { downloading = true; return new Promise<Uint8Array>(resolve => { release = resolve; }); };
    try {
        const { worker } = await start(f), before = worker.session.id;
        const extra = message({ text: '/ps waiting file', resources: [{ key: 'file', kind: 'file', name: 'fixture.txt' }] });
        await f.gateway.message(extra); await until(() => downloading);
        const control = message({ text: command, senderId: 'admin' }); await f.gateway.message(control);
        assert.equal(state(f, extra), 'cancelled'); assert.equal(worker.stopped, true);
        release(Buffer.from('late bytes'));
        const next = message({ text: 'explicit ordinary new task' }); await f.gateway.message(next);
        await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers[0]!.sent.length, 1);
        assert.equal(f.workers[1]!.sent[0]!.sessionId === before, command === '/stop');
        assert.match(reply(f, control), /补充.*取消/);
        assert.ok(!f.transport.receiptStates.some(r => r.id === extra.id && r.state === 'done'));
    } finally { release?.(Buffer.from('cleanup')); await f.close(); }
});

test('worker exit cancels queued supplements and restart never treats them as ordinary tasks', async () => {
    const f = await fixture();
    let recovered: Gateway | undefined;
    try {
        const { worker } = await start(f);
        await f.gateway.message(message({ senderId: 'admin', text: '/compact' }));
        const extra = message({ text: '/btw bound to the old run' }); await f.gateway.message(extra);
        assert.equal(state(f, extra), 'queued');
        worker.emit({ type: 'exit', code: 17, cleanupConfirmed: true });
        await until(() => state(f, extra) === 'cancelled');
        await f.gateway.close();
        recovered = new Gateway(f.config, f.store, f.transport, f.factory); await recovered.recover();
        await new Promise(resolve => setTimeout(resolve, 30));
        assert.ok(f.workers.every(w => w.sent.every(d => d.message.id !== extra.id)));
        assert.ok(!f.transport.receiptStates.some(r => r.id === extra.id && r.state === 'done'));
    } finally { await recovered?.close(); await f.close(); }
});

test('shutdown and restart discard an unsent supplement even while the selected native session survives', async () => {
    const f = await fixture();
    let recovered: Gateway | undefined;
    try {
        const { worker } = await start(f);
        await f.gateway.message(message({ senderId: 'admin', text: '/compact' }));
        const extra = message({ text: '/ps cannot survive the target run' }); await f.gateway.message(extra);
        const before = worker.session.id;
        await f.gateway.close();
        recovered = new Gateway(f.config, f.store, f.transport, f.factory); await recovered.recover();
        await until(() => state(f, extra) === 'cancelled');
        assert.equal(f.store.listSessions(worker.room.key).find(s => s.state === 'open')!.id, before);
        assert.ok(f.workers.every(w => w.sent.every(d => d.message.id !== extra.id)));
        assert.ok(f.transport.sent.some(s => s.replyTo === extra.id && JSON.stringify(s.card ?? s.text).includes('补充已取消')));
    } finally { await recovered?.close(); await f.close(); }
});

test('sent supplement outcomes are uncertain after restart and never automatically replayed', async () => {
    const f = await fixture();
    let recovered: Gateway | undefined;
    try {
        const { worker } = await start(f);
        const extra = message({ text: '/btw already handed off' }); await f.gateway.message(extra);
        await until(() => worker.sent.length === 2);
        await f.gateway.close();
        recovered = new Gateway(f.config, f.store, f.transport, f.factory); await recovered.recover();
        assert.equal(state(f, extra), 'uncertain'); assert.equal(f.workers.length, 1);
        assert.ok(!f.transport.receiptStates.some(r => r.id === extra.id && r.state === 'done'));
    } finally { await recovered?.close(); await f.close(); }
});

test('supplements obey inbox and rate limits and never answer a permission request', async () => {
    const f = await fixture();
    try {
        const { worker } = await start(f);
        worker.emit({ type: 'request', id: 'permission', kind: 'permission', name: 'Write', input: {} });
        await until(() => f.gateway.approvals.size(worker.session.id) === 1);
        f.config.limits.inboxPerRoom = 1;
        const full = message({ text: '/btw capacity blocked' }); await f.gateway.message(full);
        assert.match(reply(f, full), /上限/); assert.equal(worker.sent.length, 1);
        f.config.limits.inboxPerRoom = 64; f.config.limits.messagesPerMinute = 1;
        const rate = message({ text: '/ps rate blocked' }); await f.gateway.message(rate);
        assert.match(reply(f, rate), /频繁/); assert.equal(worker.sent.length, 1);
        f.config.limits.messagesPerMinute = 30;
        const requestId = String(f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id);
        const text = message({ text: `/btw /answer ${requestId} approve`, senderId: 'bob' }); await f.gateway.message(text);
        await until(() => worker.sent.length === 2);
        assert.equal(worker.sent[1]!.message.text, `/answer ${requestId} approve`);
        assert.equal(f.gateway.approvals.size(worker.session.id), 1); assert.equal(worker.responses.length, 0);
        assert.equal(route(f.config, text)!.key, worker.room.key);
    } finally { await f.close(); }
});
