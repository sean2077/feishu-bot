import test from 'node:test';
import assert from 'node:assert/strict';
import { setTimeout as delay } from 'node:timers/promises';
import { fixture, message } from './helpers.ts';

test('stopping a chat does not wait for a stalled attachment download', { timeout: 10000 }, async () => {
    const f = await fixture();
    let started!: () => void;
    let release!: () => void;
    const downloading = new Promise<void>(resolve => { started = resolve; });
    const stalled = new Promise<Uint8Array>(resolve => { release = () => resolve(Buffer.from('attachment')); });
    f.transport.download = async () => { started(); return stalled; };
    const incoming = message({ resources: [{ key: 'file-key', kind: 'file', name: 'fixture.txt' }] });
    let stop: Promise<void> | undefined;
    try {
        await f.gateway.message(incoming);
        await downloading;
        stop = f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        const completed = await Promise.race([stop.then(() => true), delay(2000).then(() => false)]);
        assert.equal(completed, true, 'the in-flight download must not hold the stop/dispatch lock');
        assert.equal(f.workers[0]?.sent.length, 0, 'cancelled input must never reach the native worker');
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(incoming.id)?.status, 'cancelled',
            'an explicitly stopped input must not be relabeled uncertain');
    } finally {
        release();
        await stop?.catch(() => {});
        await f.close();
    }
});
