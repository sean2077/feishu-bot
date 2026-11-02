import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { readdir } from 'node:fs/promises';
import { Gateway } from '../src/gateway.ts';
import { parseConfig } from '../src/config.ts';
import { route } from '../src/policy.ts';
import { fixture, rawConfig, message, until } from './helpers.ts';
import type { Delivery, Message } from '../src/types.ts';

type Fixture = Awaited<ReturnType<typeof fixture>>;
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));
function enable(f: Fixture, ms = 120, count = 8, bytes = 4096) {
    Object.assign(f.config.limits, { imageBatchMs: ms, imageBatchMaxImages: count, imageBatchBytes: bytes });
}
function image(overrides: Partial<Message> = {}): Message {
    const id = randomUUID(), key = `img_fixture_${id}`;
    return message({ id, text: `![image](${key})`, imageOnly: true,
        resources: [{ key, kind: 'image', name: 'fixture-image.png' }], ...overrides });
}
const members = (delivery: Delivery) => delivery.batchInputs ?? [delivery];
const ids = (delivery: Delivery) => members(delivery).map(d => d.message.id);
const state = (f: Fixture, input: Message) => f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status;
const allSent = (f: Fixture) => f.workers.flatMap(w => w.sent);
function done(f: Fixture, index = 0, text = 'images received') {
    const worker = f.workers[index]!;
    worker.emit({ type: 'result', success: true, text, uuids: worker.sent.map(d => d.uuid) });
}

test('image batching is opt-in with strict bounded configuration', async () => {
    const f = await fixture();
    try {
        assert.equal(f.config.limits.imageBatchMs, 0);
        for (const [key, value] of [['imageBatchMs', -1], ['imageBatchMs', 5001], ['imageBatchMaxImages', 1],
            ['imageBatchMaxImages', 13], ['imageBatchBytes', 0], ['imageBatchBytes', 104857601]]) {
            const raw = rawConfig(f.dir); raw.limits = { [key]: value };
            assert.throws(() => parseConfig(raw));
        }
        const raw = rawConfig(f.dir); raw.limits = { imageBatchMs: 200, imageBatchMaxImages: 4, imageBatchBytes: 1048576 };
        const config = parseConfig(raw);
        assert.equal(config.limits.imageBatchMs, 200); assert.equal(config.limits.imageBatchMaxImages, 4);
        assert.equal(config.limits.imageBatchBytes, 1048576);
    } finally { await f.close(); }
});

test('disabled batching preserves separate native inputs without introducing a waiting window', async () => {
    const f = await fixture();
    try {
        const a = image(), b = image();
        await f.gateway.message(a); await f.gateway.message(b);
        await until(() => allSent(f).length === 2);
        assert.deepEqual(allSent(f).map(d => d.message.id), [a.id, b.id]);
        assert.ok(allSent(f).every(d => !d.imageBatch && !d.batchInputs));
    } finally { await f.close(); }
});

test('window expiry sends one image input with every original UUID, resource and receipt origin', async () => {
    const f = await fixture(); enable(f);
    try {
        const a = image({ threadId: 'thread-a', replyTo: 'quote-a' }), b = image({ threadId: 'thread-a', replyTo: 'quote-a' });
        const quote = message({ id: 'quote-a', text: 'shared quote', senderId: 'bob' });
        f.transport.quoted.set(quote.id, quote);
        await f.gateway.message(a); await f.gateway.message(b);
        assert.equal(f.workers.length, 0, 'waiting images must not create a native worker');
        const room = route(f.config, a)!;
        const pending = f.store.pending(room.key);
        assert.equal(pending.length, 2); assert.ok(pending.every(d => d.imageBatch));
        assert.equal(pending[0]!.imageBatch!.id, pending[1]!.imageBatch!.id);
        assert.equal(pending[0]!.imageBatch!.deadline, pending[1]!.imageBatch!.deadline, 'the window is fixed, not extended by later images');
        assert.ok(pending.every(d => !d.batchInputs), 'wire aggregation must not replace the durable original rows');
        await until(() => allSent(f).length === 1);
        const sent = allSent(f)[0]!;
        assert.deepEqual(ids(sent), [a.id, b.id]);
        assert.deepEqual(members(sent).map(d => d.uuid), pending.map(d => d.uuid));
        assert.ok(members(sent).every(d => d.message.senderId === 'alice' && d.message.threadId === 'thread-a'));
        assert.ok(members(sent).every(d => d.attachments.length === 1 && d.quote?.id === 'quote-a'));
        f.workers[0]!.emit({ type: 'consumed', uuid: sent.uuid });
        assert.equal(state(f, a), 'consumed'); assert.equal(state(f, b), 'consumed');
        done(f);
        await until(() => [a, b].every(m => f.transport.receiptStates.some(r => r.id === m.id && r.state === 'done')));
        assert.equal(f.transport.sent.filter(s => s.card).length, 1);
        assert.equal(f.transport.sent.find(s => s.card)!.replyTo, a.id);
        assert.equal(state(f, a), 'done'); assert.equal(state(f, b), 'done');
    } finally { await f.close(); }
});

test('image count seals a batch immediately and later images start a different batch', async () => {
    const f = await fixture(); enable(f, 5000, 2);
    try {
        const inputs = [image(), image(), image(), image()];
        for (const input of inputs) await f.gateway.message(input);
        await until(() => allSent(f).length === 2);
        assert.deepEqual(allSent(f).map(ids), [inputs.slice(0, 2).map(m => m.id), inputs.slice(2).map(m => m.id)]);
        assert.notEqual(allSent(f)[0]!.imageBatch!.id, allSent(f)[1]!.imageBatch!.id);
        assert.ok(allSent(f).every(d => d.imageBatch!.closed));
    } finally { await f.close(); }
});

for (const different of [{ senderId: 'bob' }, { threadId: 'other-thread' }, { replyTo: 'other-quote' }])
    test(`image boundaries preserve ${Object.keys(different)[0]} instead of mixing input provenance`, async () => {
        const f = await fixture(); enable(f, 5000);
        try {
            const a = image(), b = image(different), text = message({ text: 'after images' });
            await f.gateway.message(a); await f.gateway.message(b); await f.gateway.message(text);
            await until(() => allSent(f).length === 3);
            assert.deepEqual(allSent(f).map(ids), [[a.id], [b.id], [text.id]]);
        } finally { await f.close(); }
    });

test('different chats keep separate image windows and workspaces', async () => {
    const f = await fixture(); enable(f, 5000, 2);
    try {
        const a = image(), b = image({ chatId: 'group2' }), c = image(), d = image({ chatId: 'group2' });
        for (const input of [a, b, c, d]) await f.gateway.message(input);
        await until(() => f.workers.length === 2 && f.workers.every(w => w.sent.length === 1));
        assert.deepEqual(ids(f.workers.find(w => w.room.chatId === 'group1')!.sent[0]!), [a.id, c.id]);
        assert.deepEqual(ids(f.workers.find(w => w.room.chatId === 'group2')!.sent[0]!), [b.id, d.id]);
        assert.notEqual(f.workers[0]!.session.id, f.workers[1]!.session.id);
    } finally { await f.close(); }
});

test('text and management messages close image windows without crossing or reordering them', async () => {
    const f = await fixture(); enable(f, 5000);
    try {
        const a = image(), b = image(), text = message({ text: 'caption boundary' }), c = image(), d = image();
        for (const input of [a, b, text, c]) await f.gateway.message(input);
        await f.gateway.message(message({ text: '/help' }));
        await f.gateway.message(d);
        const tail = message({ text: 'last text' }); await f.gateway.message(tail);
        await until(() => allSent(f).length === 5);
        assert.deepEqual(allSent(f).map(ids), [[a.id, b.id], [text.id], [c.id], [d.id], [tail.id]]);
    } finally { await f.close(); }
});

test('image batching never admits unauthorized or unmentioned messages and duplicates do not extend a batch', async () => {
    const f = await fixture(); enable(f, 5000, 2);
    try {
        const a = image(), b = image(), hidden = image({ mentioned: false }), foreign = image({ senderId: 'stranger' });
        await f.gateway.message(a);
        const room = route(f.config, a)!, deadline = f.store.pending(room.key)[0]!.imageBatch!.deadline;
        await f.gateway.message({ ...a, text: 'changed duplicate body' });
        await f.gateway.message(hidden); await f.gateway.message(foreign);
        assert.equal(f.store.pending(room.key)[0]!.imageBatch!.deadline, deadline);
        await f.gateway.message(b); await until(() => allSent(f).length === 1);
        assert.deepEqual(ids(allSent(f)[0]!), [a.id, b.id]);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n, 2);
        assert.ok(f.transport.receiptStates.every(r => [a.id, b.id].includes(r.id)));
    } finally { await f.close(); }
});

test('an open image window does not delay permission callbacks', async () => {
    const f = await fixture(); enable(f, 5000);
    try {
        await f.gateway.message(message({ text: 'active task' })); await until(() => allSent(f).length === 1);
        const worker = f.workers[0]!;
        worker.emit({ type: 'request', id: 'permission', kind: 'permission', name: 'Write', input: {} });
        await until(() => f.gateway.approvals.size(worker.session.id) === 1 && f.transport.sent.some(s => s.card));
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b);
        const request = String(f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id);
        const card = f.transport.sent.find(s => s.card)!;
        assert.match(await f.gateway.approvals.click(worker.room.chatId, 'admin', { request, decision: 'allow' }, card.id), /已批准/);
        await until(() => worker.responses.some(r => r.id === 'permission' && r.value.approved === true));
        assert.equal(state(f, a), 'queued'); assert.equal(state(f, b), 'queued');
        assert.equal(worker.sent.length, 1);
    } finally { await f.close(); }
});

test('compact remains between earlier and later image batches while foreground work is active', async () => {
    const f = await fixture(); enable(f, 5000, 2);
    try {
        await f.gateway.message(message({ text: 'active task' })); await until(() => allSent(f).length === 1);
        const worker = f.workers[0]!, original = worker.sent[0]!;
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b);
        await until(() => worker.sent.length === 2);
        const before = worker.sent[1]!;
        const compact = message({ text: '/compact', senderId: 'admin' }); await f.gateway.message(compact);
        const c = image(), d = image(); await f.gateway.message(c); await f.gateway.message(d);
        assert.equal(worker.sent.length, 2); assert.equal(state(f, c), 'queued');
        worker.emit({ type: 'result', success: true, text: 'foreground finished', uuids: [original.uuid, before.uuid] });
        await until(() => worker.sent.length === 3);
        const control = worker.sent[2]!; assert.equal(control.command?.name, 'compact');
        assert.equal(state(f, c), 'queued'); assert.equal(state(f, d), 'queued');
        worker.emit({ type: 'result', success: true, text: '', uuid: control.uuid, compact: 'compacted' });
        await until(() => worker.sent.length === 4);
        assert.deepEqual(ids(worker.sent[3]!), [c.id, d.id]);
    } finally { await f.close(); }
});

for (const failure of ['single-file', 'total-bytes', 'download']) test(`a ${failure} failure rejects the whole unsent image batch without partial execution`, async () => {
    const f = await fixture(); enable(f, 5000, 2, failure === 'total-bytes' ? 15 : 64);
    f.config.limits.attachmentBytes = failure === 'single-file' ? 8 : 16;
    let downloads = 0;
    f.transport.download = async () => { downloads++; if (failure === 'download' && downloads === 2) throw new Error('PRIVATE_PROVIDER_BODY'); return Buffer.alloc(10); };
    try {
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b);
        await until(() => [a, b].every(m => state(f, m) === 'cancelled'));
        assert.equal(allSent(f).length, 0); assert.ok(!f.transport.receiptStates.some(r => r.state === 'done'));
        await until(() => f.transport.sent.some(s => JSON.stringify(s.card).includes('图片批次未提交')));
        assert.ok(!JSON.stringify(f.transport.sent).includes('PRIVATE_PROVIDER_BODY'));
        assert.ok(!(await readdir(f.dir, { recursive: true })).some(path => String(path).endsWith('.png')));
    } finally { await f.close(); }
});

for (const control of ['/stop', '/new']) test(`${control} cancels a pending batch and a late download cannot revive it`, async () => {
    const f = await fixture(); enable(f, 5000, 2);
    let release!: (value: Uint8Array) => void, waiting = false;
    f.transport.download = async () => { waiting = true; return new Promise<Uint8Array>(resolve => { release = resolve; }); };
    try {
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b); await until(() => waiting);
        const oldSession = f.workers[0]!.session.id;
        await f.gateway.message(message({ senderId: 'admin', text: control }));
        assert.equal(state(f, a), 'cancelled'); assert.equal(state(f, b), 'cancelled');
        release(Buffer.from('late fixture'));
        const next = message({ text: 'explicit next task' }); await f.gateway.message(next);
        await until(() => allSent(f).length === 1);
        assert.deepEqual(ids(allSent(f)[0]!), [next.id]);
        assert.equal(allSent(f)[0]!.sessionId === oldSession, control === '/stop');
        assert.ok(!f.transport.receiptStates.some(r => [a.id, b.id].includes(r.id) && r.state === 'done'));
    } finally { release?.(Buffer.from('cleanup')); await f.close(); }
});

test('revocation during batch preparation discards all members before model input', async () => {
    const f = await fixture(); enable(f, 5000, 2);
    let release!: (value: Uint8Array) => void, waiting = false;
    f.transport.download = async () => { waiting = true; return new Promise<Uint8Array>(resolve => { release = resolve; }); };
    try {
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b); await until(() => waiting);
        f.config.groups[0]!.members = ['bob'];
        release(Buffer.from('downloaded before revocation was noticed'));
        await until(() => [a, b].every(m => state(f, m) === 'cancelled'));
        assert.equal(allSent(f).length, 0); assert.equal(f.transport.sent.length, 0);
        const bodies = f.store.db.prepare('SELECT message,payload FROM inbox WHERE id IN (?,?)').all(a.id, b.id);
        assert.ok(bodies.every(r => r.message === '{}' && r.payload === null));
        assert.ok(!f.transport.receiptStates.some(r => r.state === 'done'));
    } finally { release?.(Buffer.from('cleanup')); await f.close(); }
});

test('a queued image window survives a gateway restart with the same session and original members', async () => {
    const f = await fixture(); enable(f, 180);
    let recovered: Gateway | undefined;
    try {
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b);
        const room = route(f.config, a)!, pending = f.store.pending(room.key), session = pending[0]!.sessionId;
        await f.gateway.close(); assert.equal(f.workers.length, 0);
        recovered = new Gateway(f.config, f.store, f.transport, f.factory); await recovered.recover();
        await until(() => allSent(f).length === 1);
        assert.deepEqual(ids(allSent(f)[0]!), [a.id, b.id]); assert.equal(allSent(f)[0]!.sessionId, session);
        assert.deepEqual(members(allSent(f)[0]!).map(d => d.uuid), pending.map(d => d.uuid));
    } finally { await recovered?.close(); await f.close(); }
});

test('sent image batches become uncertain as a unit after restart and are never automatically replayed', async () => {
    const f = await fixture(); enable(f, 5000, 2);
    let recovered: Gateway | undefined;
    try {
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b); await until(() => allSent(f).length === 1);
        await f.gateway.close();
        recovered = new Gateway(f.config, f.store, f.transport, f.factory); await recovered.recover();
        assert.equal(state(f, a), 'uncertain'); assert.equal(state(f, b), 'uncertain');
        await sleep(30); assert.equal(allSent(f).length, 1); assert.equal(f.workers.length, 1);
        assert.ok(!f.transport.receiptStates.some(r => r.state === 'done'));
    } finally { await recovered?.close(); await f.close(); }
});

test('a clean unexpected worker exit can resume never-sent images in the same native session, not replay old work', async () => {
    const f = await fixture(); enable(f, 150);
    try {
        const original = message({ text: 'active task' }); await f.gateway.message(original); await until(() => allSent(f).length === 1);
        const worker = f.workers[0]!, nativeId = randomUUID(); worker.emit({ type: 'ready', ccId: nativeId });
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b);
        worker.emit({ type: 'exit', code: 17, cleanupConfirmed: true });
        await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers[1]!.session.id, worker.session.id); assert.equal(f.workers[1]!.session.ccId, nativeId);
        assert.deepEqual(ids(f.workers[1]!.sent[0]!), [a.id, b.id]); assert.equal(state(f, original), 'uncertain');
        assert.equal(allSent(f).filter(d => d.message.id === original.id).length, 1);
    } finally { await f.close(); }
});

test('new session before window expiry never inherits an old image batch', async () => {
    const f = await fixture(); enable(f, 5000);
    try {
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b);
        const room = route(f.config, a)!, old = f.store.pending(room.key)[0]!.sessionId;
        await f.gateway.message(message({ senderId: 'admin', text: '/new' }));
        assert.equal(f.workers.length, 0); assert.equal(state(f, a), 'cancelled'); assert.equal(state(f, b), 'cancelled');
        enable(f, 5000, 2);
        const c = image(), d = image(); await f.gateway.message(c); await f.gateway.message(d);
        await until(() => allSent(f).length === 1);
        assert.deepEqual(ids(allSent(f)[0]!), [c.id, d.id]); assert.notEqual(allSent(f)[0]!.sessionId, old);
    } finally { await f.close(); }
});

test('all image origins wait for the required final-answer file before any Done receipt', async () => {
    const f = await fixture(); enable(f, 5000, 2);
    const file = f.transport.file.bind(f.transport); let fail = true;
    f.transport.file = async (...args) => { if (fail) throw new Error('fixture upload outcome unknown'); return file(...args); };
    try {
        const a = image(), b = image(); await f.gateway.message(a); await f.gateway.message(b); await until(() => allSent(f).length === 1);
        done(f, 0, 'long answer '.repeat(5000));
        await until(() => f.store.db.prepare("SELECT count(*) AS n FROM outbox WHERE state='uncertain'").get()!.n === 1);
        assert.ok(!f.transport.receiptStates.some(r => r.state === 'done'));
        const row = f.store.db.prepare("SELECT id,origins FROM outbox WHERE kind='reply'").get()!;
        assert.deepEqual(JSON.parse(String(row.origins)).map((o: { id: string }) => o.id), [a.id, b.id]);
        fail = false;
        await f.gateway.message(message({ senderId: 'admin', text: `/retry-output ${row.id}` }));
        await until(() => [a, b].every(m => f.transport.receiptStates.some(r => r.id === m.id && r.state === 'done')));
        assert.equal(allSent(f).length, 1, 'output retry must not rerun the image task');
    } finally { await f.close(); }
});
