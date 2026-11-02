import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { Gateway } from '../src/gateway.ts';
import type { WorkerEvent } from '../src/types.ts';
import { fixture, message, until } from './helpers.ts';

/** Separate pipe-write completion from the worker's actual input acknowledgement. */
async function handoff() {
    const f = await fixture();
    await f.gateway.close();
    const events: ((event: WorkerEvent) => void)[] = [];
    const gateway = new Gateway(f.config, f.store, f.transport, async (room, session, emit, workspace) => {
        events.push(emit);
        return f.factory(room, session, event => { if (event.type !== 'accepted') emit(event); }, workspace);
    });
    const first = message({ text: 'foreground task' });
    await gateway.message(first); await until(() => f.workers[0]?.sent.length === 1);
    const input = message({ senderId: 'bob', text: '/btw extra facts' });
    await gateway.message(input); await until(() => f.workers[0]?.sent.length === 2);
    const worker = f.workers[0]!, delivery = worker.sent[1]!;
    const state = () => String(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)!.status);
    const acknowledge = () => events[0]!({ type: 'accepted', uuid: delivery.uuid });
    const reject = () => events[0]!({ type: 'input_rejected', uuid: delivery.uuid, reason: 'foreground_ended' });
    const confirmations = () => f.transport.sent.filter(s => s.replyTo === input.id && s.text?.includes('补充已交给原生输入流'));
    return { ...f, gateway, first, input, worker, delivery, events, state, acknowledge, reject, confirmations,
        close: async () => { await gateway.close(); await f.close(); } };
}

test('supplement confirmation waits for the worker acknowledgement and duplicate acknowledgements are idempotent', async () => {
    const f = await handoff();
    try {
        assert.equal(f.state(), 'sent');
        assert.equal(f.confirmations().length, 0, 'pipe write completion is not native queue acceptance');
        f.acknowledge(); f.acknowledge();
        assert.equal(f.confirmations().length, 1);
        f.reject();
        assert.equal(f.state(), 'sent', 'a contradictory rejection cannot cancel a confirmed native enqueue');
        f.events[0]!({ type: 'result', success: true, text: 'complete answer', resultId: randomUUID(),
            uuids: f.worker.sent.map(d => d.uuid) });
        await until(() => [f.first, f.input].every(m => f.transport.receiptStates.some(r => r.id === m.id && r.state === 'done')));
        assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM outbox WHERE kind='reply'").get()!.n, 1);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM usage_results').get()!.n, 1);
    } finally { await f.close(); }
});

test('proven pre-enqueue rejection cancels only its supplement once and retains the native session', async () => {
    const f = await handoff();
    try {
        const nativeId = randomUUID(); f.events[0]!({ type: 'ready', ccId: nativeId });
        f.events[0]!({ type: 'input_rejected', uuid: f.worker.sent[0]!.uuid, reason: 'foreground_ended' });
        f.events[0]!({ type: 'input_rejected', uuid: randomUUID(), reason: 'foreground_ended' });
        assert.equal(f.state(), 'sent');
        f.reject(); f.reject(); f.acknowledge();
        assert.equal(f.state(), 'cancelled'); assert.equal(f.confirmations().length, 0);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(f.first.id)!.status, 'sent');
        await until(() => f.transport.sent.some(s => s.replyTo === f.input.id && JSON.stringify(s.card).includes('补充已取消')));
        assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM outbox WHERE kind='recovery'").get()!.n, 1);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM usage_results').get()!.n, 0);
        assert.ok(!f.transport.receiptStates.some(r => r.id === f.input.id && r.state === 'done'));
        f.events[0]!({ type: 'result', success: true, text: 'original complete', uuid: f.worker.sent[0]!.uuid, resultId: randomUUID() });
        await until(() => f.store.countPending(f.worker.room.key) === 0);
        await f.gateway.message(message({ text: 'explicit next task' }));
        await until(() => f.worker.sent.length === 3);
        assert.equal(f.workers.length, 1);
        assert.equal(f.worker.sent[2]!.sessionId, f.worker.session.id);
        assert.equal(f.store.db.prepare('SELECT cc FROM sessions WHERE id=?').get(f.worker.session.id)!.cc, nativeId);
    } finally { await f.close(); }
});

for (const command of ['/stop', '/new']) test(`${command} wins late supplement acknowledgement and rejection frames`, async () => {
    const f = await handoff();
    try {
        const oldSession = f.worker.session.id;
        await f.gateway.message(message({ senderId: 'admin', text: command }));
        await f.gateway.message(message({ text: 'explicit continuation' }));
        await until(() => f.workers[1]?.sent.length === 1);
        f.acknowledge(); f.reject();
        assert.equal(f.state(), 'cancelled'); assert.equal(f.confirmations().length, 0);
        assert.equal(f.workers[1]!.session.id === oldSession, command === '/stop');
        assert.ok(!f.transport.sent.some(s => s.replyTo === f.input.id));
        assert.ok(!f.transport.receiptStates.some(r => r.id === f.input.id && r.state === 'done'));
        assert.equal(f.workers[1]!.sent.length, 1);
    } finally { await f.close(); }
});

test('a worker-rejected supplement is discarded silently after its author is revoked', async () => {
    const f = await handoff();
    try {
        f.config.groups[0]!.members = ['alice'];
        f.reject();
        const row = f.store.db.prepare('SELECT status,message,payload FROM inbox WHERE id=?').get(f.input.id)!;
        assert.equal(row.status, 'cancelled'); assert.equal(row.message, '{}'); assert.equal(row.payload, null);
        assert.ok(!f.transport.sent.some(s => s.replyTo === f.input.id));
        assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM outbox WHERE kind='recovery'").get()!.n, 0);
        assert.equal(f.confirmations().length, 0);
    } finally { await f.close(); }
});

test('rejection feedback persistence failure retains the sent input instead of losing cancellation evidence', async () => {
    const f = await handoff();
    try {
        f.store.db.exec("CREATE TEMP TRIGGER reject_notice BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'fixture notice failure'); END");
        assert.throws(f.reject, /fixture notice failure/);
        assert.equal(f.state(), 'sent');
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM outbox').get()!.n, 0);
        assert.equal(f.confirmations().length, 0);
        f.store.db.exec('DROP TRIGGER reject_notice');
        // A host/runtime failure cannot reinterpret the uncommitted proof as permission to retry.
        await f.gateway.close();
        const recovered = new Gateway(f.config, f.store, f.transport, f.factory);
        try {
            await recovered.recover();
            assert.equal(f.state(), 'uncertain');
            assert.equal(f.workers.length, 1); assert.equal(f.worker.sent.length, 2);
            assert.ok(!f.transport.receiptStates.some(r => r.id === f.input.id && r.state === 'done'));
        } finally { await recovered.close(); }
    } finally { await f.close(); }
});
