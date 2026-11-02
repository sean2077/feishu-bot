import test from 'node:test';
import assert from 'node:assert/strict';
import { rm, stat } from 'node:fs/promises';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { Store } from '../src/store.ts';
import { Gateway } from '../src/gateway.ts';
import { fixture, message, until } from './helpers.ts';

function heldCall() {
    let release!: () => void;
    const promise = new Promise<void>(resolve => { release = resolve; });
    return { promise, release: () => release() };
}

// A remote create can succeed after shutdown. Closing locally must neither
// wait forever nor turn an unknown external write into a retryable create.
test('stalled durable final-card creation does not hold shutdown or replay after restart', { timeout: 15000 }, async () => {
    const f = await fixture();
    const held = heldCall();
    let creates = 0, closed = false;
    let closeTask: Promise<void> | undefined;
    let reopened: Store | undefined, recovered: Gateway | undefined;
    try {
        f.transport.card = async () => { creates++; await held.promise; return 'late-final-card'; };
        const input = message({ text: 'final answer without a preview' });
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        worker.emit({ type: 'result', success: true, text: 'persisted final text', uuid: worker.sent[0]!.uuid });
        await until(() => creates === 1 && f.store.db.prepare("SELECT state FROM outbox WHERE kind='reply'").get()?.state === 'sending');
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'done');

        closeTask = f.gateway.close();
        assert.equal(await Promise.race([closeTask.then(() => true), delay(1400).then(() => false)]), true,
            'Gateway.close must not wait indefinitely for the durable outbox create');
        const row = f.store.db.prepare("SELECT id,state,target,text FROM outbox WHERE kind='reply'").get()!;
        assert.equal(row.text, 'persisted final text');
        assert.equal(row.target, null);
        assert.equal(row.state, 'uncertain', 'a possibly accepted create must not be auto-retried');
        assert.equal(creates, 1);
        assert.ok(!f.transport.receiptStates.some(r => r.id === input.id && r.state === 'done'));

        f.store.close(); closed = true;
        held.release();
        await delay(35); // A remote result after local close must not touch closed SQLite.

        reopened = new Store(join(f.dir, 'state.sqlite'));
        recovered = new Gateway(f.config, reopened, f.transport, f.factory);
        await recovered.recover();
        await recovered.sweep();
        assert.equal(reopened.db.prepare('SELECT state FROM outbox WHERE id=?').get(row.id)?.state, 'uncertain');
        assert.equal(reopened.db.prepare('SELECT target FROM outbox WHERE id=?').get(row.id)?.target, null);
        assert.equal(creates, 1, 'recovery must never blind-create a second remote card');
    } finally {
        held.release();
        await closeTask?.catch(() => {});
        await recovered?.close();
        reopened?.close();
        if (!closed) { await f.gateway.close().catch(() => {}); f.store.close(); }
        await rm(f.dir, { recursive: true, force: true });
    }
});

test('stalled required full-answer upload releases shutdown but retains unknown file intent', { timeout: 15000 }, async () => {
    const f = await fixture();
    const held = heldCall();
    let uploads = 0, closed = false;
    let closeTask: Promise<void> | undefined;
    let reopened: Store | undefined, recovered: Gateway | undefined;
    try {
        f.transport.file = async () => { uploads++; await held.promise; return 'late-full-answer-file'; };
        const input = message({ text: 'write a long answer' });
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        worker.emit({ type: 'result', success: true, text: 'A'.repeat(25000), uuid: worker.sent[0]!.uuid });
        await until(() => uploads === 1);
        const before = f.store.db.prepare("SELECT state,target,file_state,file_path FROM outbox WHERE kind='reply'").get()!;
        assert.equal(before.state, 'sending');
        assert.ok(before.target, 'the reply card is already confirmed before the required file upload');
        assert.equal(before.file_state, 'sending');
        await stat(String(before.file_path));

        closeTask = f.gateway.close();
        assert.equal(await Promise.race([closeTask.then(() => true), delay(1400).then(() => false)]), true,
            'a pending remote upload must not prevent database release');
        const row = f.store.db.prepare("SELECT id,state,target,file_state,file_target,file_path FROM outbox WHERE kind='reply'").get()!;
        assert.equal(row.state, 'uncertain');
        assert.equal(row.target, before.target, 'the already-known card ID must remain intact');
        assert.equal(row.file_state, 'uncertain');
        assert.equal(row.file_target, null);
        assert.equal(uploads, 1);
        assert.ok(!f.transport.receiptStates.some(r => r.id === input.id && r.state === 'done'));

        f.store.close(); closed = true;
        held.release();
        await delay(35);
        reopened = new Store(join(f.dir, 'state.sqlite'));
        recovered = new Gateway(f.config, reopened, f.transport, f.factory);
        await recovered.recover();
        await recovered.sweep();
        assert.equal(reopened.db.prepare('SELECT state FROM outbox WHERE id=?').get(row.id)?.state, 'uncertain');
        assert.equal(reopened.db.prepare('SELECT file_state FROM outbox WHERE id=?').get(row.id)?.file_state, 'uncertain');
        assert.equal(uploads, 1, 'an ambiguous file upload is not repeated without explicit action');
        await stat(String(row.file_path)); // Keep the durable bytes for a deliberate retry decision.
    } finally {
        held.release();
        await closeTask?.catch(() => {});
        await recovered?.close();
        reopened?.close();
        if (!closed) { await f.gateway.close().catch(() => {}); f.store.close(); }
        await rm(f.dir, { recursive: true, force: true });
    }
});

test('stalled durable update keeps known card ID for update-only recovery', { timeout: 15000 }, async () => {
    const f = await fixture();
    const held = heldCall();
    let updates = 0, closed = false;
    let closeTask: Promise<void> | undefined;
    let reopened: Store | undefined, recovered: Gateway | undefined;
    try {
        const input = message({ text: 'stream then finish' });
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        worker.emit({ type: 'text', partial: false, text: 'preview', turnId: uuid, blockId: 'text:0' });
        await until(() => f.transport.sent.some(m => m.replyTo === input.id && !!m.card), 4500);
        const target = f.transport.sent.find(m => m.replyTo === input.id && !!m.card)!.id;
        f.transport.update = async () => { updates++; await held.promise; };
        worker.emit({ type: 'result', success: true, text: 'durable full answer', uuid });
        await until(() => updates === 1 && f.store.db.prepare("SELECT state FROM outbox WHERE kind='reply'").get()?.state === 'sending');

        closeTask = f.gateway.close();
        assert.equal(await Promise.race([closeTask.then(() => true), delay(1400).then(() => false)]), true,
            'a known-card update can be safely retried later without waiting for the stalled call');
        const row = f.store.db.prepare("SELECT id,state,target FROM outbox WHERE kind='reply'").get()!;
        assert.equal(row.target, target);
        assert.equal(row.state, 'pending', 'a persisted remote ID permits only an update, never a new create');
        assert.equal(f.transport.sent.filter(m => m.card).length, 1);
        f.store.close(); closed = true;
        held.release();
        await delay(35);

        reopened = new Store(join(f.dir, 'state.sqlite'));
        recovered = new Gateway(f.config, reopened, f.transport, f.factory);
        f.transport.update = async () => { updates++; };
        await recovered.recover();
        await recovered.sweep();
        await until(() => reopened!.db.prepare('SELECT state FROM outbox WHERE id=?').get(row.id)?.state === 'done');
        assert.equal(f.transport.sent.filter(m => m.card).length, 1);
        assert.equal(updates, 2, 'recovery updates exactly the original confirmed card');
    } finally {
        held.release();
        await closeTask?.catch(() => {});
        await recovered?.close();
        reopened?.close();
        if (!closed) { await f.gateway.close().catch(() => {}); f.store.close(); }
        await rm(f.dir, { recursive: true, force: true });
    }
});
