import test from 'node:test';
import assert from 'node:assert/strict';
import { Outbox } from '../src/outbox.ts';
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
