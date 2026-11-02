import test from 'node:test';
import assert from 'node:assert/strict';
import { rm } from 'node:fs/promises';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { Store } from '../src/store.ts';
import { Receipts } from '../src/receipts.ts';
import { fixture, message, until } from './helpers.ts';

// A real Gateway/SQLite with a deliberately unresolved reaction API call.
// The test has no live Feishu connection; the persisted attempt must survive
// without treating an unobserved remote add as either success or failure.
test('shutdown releases a stalled reaction and recovery reconciles its uncertain add', { timeout: 12000 }, async () => {
    const f = await fixture();
    const original = message({ text: 'an unfinished turn' });
    let release!: () => void;
    const held = new Promise<void>(resolve => { release = resolve; });
    const sent: { id: string; state: string; applied?: string }[] = [];
    f.transport.receipt = async (id, state = 'processing', applied) => {
        sent.push({ id, state, applied });
        await held;
    };
    let closeTask: Promise<void> | undefined;
    let reopened: Store | undefined;
    let recovered: Receipts | undefined;
    let sourceClosed = false;
    try {
        await f.gateway.message(original);
        await until(() => f.workers[0]?.sent.length === 1 && sent.length === 1);
        const before = f.store.db.prepare('SELECT desired,applied FROM receipts WHERE id=?').get(original.id);
        assert.equal(before?.desired, 'processing');
        assert.equal(before?.applied, 'attempt:processing', 'record unknown add before starting Feishu I/O');

        closeTask = f.gateway.close();
        const finished = await Promise.race([closeTask.then(() => true), delay(1200).then(() => false)]);
        assert.equal(finished, true, 'a stalled reaction must not hold native shutdown and database release');
        assert.equal(f.workers[0]?.stopped, true);
        const pending = f.store.db.prepare('SELECT desired,applied FROM receipts WHERE id=?').get(original.id);
        assert.equal(pending?.desired, 'clear');
        assert.equal(pending?.applied, 'attempt:processing', 'unknown remote add cannot be claimed applied');
        assert.equal(sent.length, 1, 'shutdown cannot start a second reaction while the first result is unknown');

        f.store.close();
        sourceClosed = true;
        release(); // A late network completion must never touch the closed source database.
        await delay(30);

        reopened = new Store(join(f.dir, 'state.sqlite'));
        const replay: { id: string; state: string; applied?: string }[] = [];
        f.transport.receipt = async (id, state = 'processing', applied) => {
            replay.push({ id, state, applied });
        };
        recovered = new Receipts(reopened, f.transport);
        recovered.recover();
        await until(() => reopened!.db.prepare('SELECT applied FROM receipts WHERE id=?').get(original.id)?.applied === 'clear');
        assert.deepEqual(replay, [{ id: original.id, state: 'clear', applied: 'attempt:processing' }],
            'recovery must reconcile the owned uncertain add, never issue a blind second add');
        assert.equal(reopened.db.prepare('SELECT desired FROM receipts WHERE id=?').get(original.id)?.desired, 'clear');
    } finally {
        release?.();
        await closeTask?.catch(() => {});
        await recovered?.drain();
        reopened?.close();
        if (!sourceClosed) f.store.close();
        await rm(f.dir, { recursive: true, force: true });
    }
});
