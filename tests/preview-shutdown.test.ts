import test from 'node:test';
import assert from 'node:assert/strict';
import { rm } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import { fixture, message, until } from './helpers.ts';

function heldCall() {
    let release!: () => void;
    const promise = new Promise<void>(resolve => { release = resolve; });
    return { promise, release: () => release() };
}

// An accepted remote create can complete after the local service stops. The
// shutdown contract is to stop waiting locally, never to claim remote cancellation.
test('a stalled preview card create cannot block /stop or the database release', { timeout: 15000 }, async () => {
    const f = await fixture();
    const pending = heldCall();
    let created = 0;
    let stopTask: Promise<void> | undefined;
    let closed = false;
    try {
        f.transport.card = async () => { created++; await pending.promise; return 'late-preview-card'; };
        const original = message({ text: 'a long foreground task' });
        await f.gateway.message(original);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        const session = worker.session.id;
        worker.emit({ type: 'text', text: 'preview content', partial: true, blockId: 'preview:0', turnId: uuid });
        await until(() => created === 1, 4500);

        stopTask = f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        assert.equal(await Promise.race([stopTask.then(() => true), delay(1800).then(() => false)]), true,
            'a preview network create must not retain the room stop barrier');
        assert.equal(worker.stopped, true);
        assert.equal(f.store.session(worker.room.key).id, session);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(original.id)?.status, 'cancelled');
        assert.ok(!f.transport.receiptStates.some(r => r.id === original.id && r.state === 'done'));
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM outputs WHERE id=?').get('late-preview-card')?.n, 0);

        await f.gateway.close();
        f.store.close(); closed = true;
        pending.release();
        await delay(35); // A late create result must not reopen or write the closed store.
    } finally {
        pending.release();
        await stopTask?.catch(() => {});
        if (!closed) { await f.gateway.close().catch(() => {}); f.store.close(); }
        await rm(f.dir, { recursive: true, force: true });
    }
});

test('shutdown during a final result and uncertain preview create preserves the durable reply', { timeout: 15000 }, async () => {
    const f = await fixture();
    const pending = heldCall();
    let created = 0;
    let closeTask: Promise<void> | undefined;
    let closed = false;
    try {
        f.transport.card = async () => { created++; await pending.promise; return 'remote-maybe-created'; };
        const original = message({ text: 'produce a final answer' });
        await f.gateway.message(original);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        worker.emit({ type: 'text', text: 'streamed', partial: true, blockId: 'preview:0', turnId: uuid });
        await until(() => created === 1, 4500);
        worker.emit({ type: 'result', text: 'complete durable answer', success: true, uuids: [uuid] });
        await until(() => !!f.store.db.prepare("SELECT id FROM outbox WHERE state='preparing'").get());

        closeTask = f.gateway.close();
        assert.equal(await Promise.race([closeTask.then(() => true), delay(1800).then(() => false)]), true,
            'final persistence must not force shutdown to await an unknown remote preview create');
        const row = f.store.db.prepare('SELECT text,state,target FROM outbox').get();
        assert.equal(row?.text, 'complete durable answer');
        assert.equal(row?.state, 'uncertain', 'an ambiguous preview create must never become retryable as a new card');
        assert.equal(row?.target, null);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(original.id)?.status, 'done',
            'durable final answer and input completion remain one transaction');
        assert.equal(created, 1);
        assert.ok(!f.transport.receiptStates.some(r => r.id === original.id && r.state === 'done'));
        f.store.close(); closed = true;
        pending.release();
        await delay(35);
    } finally {
        pending.release();
        await closeTask?.catch(() => {});
        if (!closed) { await f.gateway.close().catch(() => {}); f.store.close(); }
        await rm(f.dir, { recursive: true, force: true });
    }
});
