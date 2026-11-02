import test from 'node:test';
import assert from 'node:assert/strict';
import { Receipts } from '../src/receipts.ts';
import { route } from '../src/policy.ts';
import { fixture, message } from './helpers.ts';

test('receipt recovery retries at most 50 entries per sweep and rotates to newer work', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        const session = f.store.session(room.key);
        const add = f.store.db.prepare(
            "INSERT INTO receipts(id,room,session,desired,applied,at,sender) VALUES(?,?,?,'clear','processing',?,'alice')"
        );
        for (let i = 0; i < 120; i++)
            add.run('old-' + String(i).padStart(3, '0'), room.key, session.id, 1000 + i);

        const receipts = new Receipts(f.store, f.transport);
        for (const expected of [50, 100, 120]) {
            receipts.retry();
            await receipts.drain();
            assert.equal(f.transport.receiptStates.length, expected,
                'one sweep must never trigger more than 50 Feishu reaction operations');
        }
        assert.equal(new Set(f.transport.receipts).size, 120, 'all old receipts must eventually be handled');

        // After reaching the end of the cursor, wrap to newer pending work.
        // The wrap path must honor the same limit as the cursor path.
        for (let i = 0; i < 60; i++)
            add.run('new-' + String(i).padStart(3, '0'), room.key, session.id, 200 + i);
        for (const expected of [170, 180]) {
            receipts.retry();
            await receipts.drain();
            assert.equal(f.transport.receiptStates.length, expected,
                'cursor wrap must also select no more than 50 receipts');
        }
        assert.equal(new Set(f.transport.receipts).size, 180, 'no receipt is retried unnecessarily');
    } finally {
        await f.close();
    }
});
