import test from 'node:test';
import assert from 'node:assert/strict';
import { Outbox } from '../src/outbox.ts';
import { Gateway } from '../src/gateway.ts';
import { route } from '../src/policy.ts';
import { fixture, message } from './helpers.ts';

test('old retryable card failures do not starve later pending outputs', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        const session = f.store.session(room.key);
        const outbox = new Outbox(f.store, f.transport);
        const ids: string[] = [];
        for (let i = 0; i < 12; i++) {
            const id = outbox.save(room, session.id, 'answer', true, 'known-card-' + i);
            // Identical runner clocks must not determine which outputs are oldest.
            f.store.db.prepare('UPDATE outbox SET at=? WHERE id=?').run(i + 1000, id);
            ids.push(id);
        }
        const attempted: string[] = [];
        f.transport.update = async id => {
            attempted.push(id);
            if (id !== 'known-card-10' && id !== 'known-card-11')
                throw new Error('fixture: transient failure updating known card');
        };
        // These are the same bounded selections used by the normal housekeeping sweep.
        for (let sweep = 0; sweep < 3; sweep++)
            for (const id of outbox.retryable(room.key))
                await outbox.deliver(id, room);
        assert.deepEqual(attempted.filter(id => id === 'known-card-10' || id === 'known-card-11'),
            ['known-card-10', 'known-card-11'], 'later known-ID updates must eventually be attempted');
        for (const id of ids.slice(10))
            assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(id)?.state, 'done');
        for (const id of ids.slice(0, 10))
            assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(id)?.state, 'pending');
    } finally {
        await f.close();
    }
});

test('retry cursor advances through equal timestamps without retrying ambiguous creates', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        const session = f.store.session(room.key);
        const outbox = new Outbox(f.store, f.transport);
        const targets = new Map<string, string>();
        for (let i = 0; i < 12; i++) {
            const target = 'known-' + i;
            const id = outbox.save(room, session.id, 'answer', true, target);
            f.store.db.prepare('UPDATE outbox SET at=? WHERE id=?').run(1000, id);
            targets.set(id, target);
        }
        const order = [...targets.keys()].sort();
        const later = new Set(order.slice(10).map(id => targets.get(id)!));
        const attempted: string[] = [];
        f.transport.update = async id => {
            attempted.push(id);
            if (!later.has(id)) throw new Error('fixture: old update failed');
        };
        const ambiguous = outbox.save(room, session.id, 'unknown create', true);
        f.store.db.prepare("UPDATE outbox SET state='uncertain',at=? WHERE id=?").run(999, ambiguous);
        for (let sweep = 0; sweep < 3; sweep++)
            for (const id of outbox.retryable(room.key))
                await outbox.deliver(id, room);
        assert.deepEqual(attempted.filter(id => later.has(id)).sort(), [...later].sort());
        for (const id of order.slice(10))
            assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(id)?.state, 'done');
        assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(ambiguous)?.state, 'uncertain');
        assert.ok(!outbox.retryable(room.key).includes(ambiguous), 'unknown create is never automatically retried');
    } finally {
        await f.close();
    }
});

test('a final reply committed before its first card create resumes safely after a restart', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        const session = f.store.session(room.key);
        const outbox = new Outbox(f.store, f.transport);
        // The model result is already durable. The process stops after saving
        // the final reply but BEFORE deliver() marks its first external attempt.
        const unsent = outbox.save(room, session.id, 'durable answer', true);
        const ambiguous = outbox.save(room, session.id, 'possibly created already', true);
        f.store.db.prepare("UPDATE outbox SET state='uncertain' WHERE id=?").run(ambiguous);

        await f.gateway.close();
        // Reconstruct the gateway/Outbox over the same SQLite database to
        // exercise the actual startup recovery path, not only an in-memory retry.
        const restarted = new Gateway(f.config, f.store, f.transport, f.factory);
        try {
            await restarted.recover();
            assert.equal(f.transport.sent.length, 0, 'recovery must not blindly create unrelated cards');
            await restarted.sweep();
            const saved = f.store.db.prepare('SELECT state,target FROM outbox WHERE id=?').get(unsent);
            assert.equal(saved?.state, 'done', 'a never-attempted final card must not be stranded');
            assert.ok(saved?.target, 'remote message ID is persisted');
            assert.equal(f.transport.sent.filter(row => row.card).length, 1);
            assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(ambiguous)?.state, 'uncertain');

            await restarted.sweep();
            assert.equal(f.transport.sent.filter(row => row.card).length, 1, 'neither completed nor ambiguous cards are recreated');
        } finally {
            await restarted.close();
        }
    } finally {
        await f.close();
    }
});
