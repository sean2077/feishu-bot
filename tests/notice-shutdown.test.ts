import test from 'node:test';
import assert from 'node:assert/strict';
import { setTimeout as delay } from 'node:timers/promises';
import { fixture, message, until } from './helpers.ts';

// Real Gateway/SQLite and a deliberately unresolved Feishu informational send.
// No live Feishu, model or native CC process is represented by this fixture.
for (const kind of ['status', 'invalid input'] as const) {
    test(`shutdown does not wait for a stalled ${kind} notice or index a late reply`, { timeout: 12000 }, async () => {
        const f = await fixture();
        const blocked = kind === 'status' ? message({ text: '/status' })
            : message({ text: 'incomplete', inputIssue: '附件不完整' });
        let signalStart!: () => void;
        let release!: () => void;
        const started = new Promise<void>(resolve => { signalStart = resolve; });
        const held = new Promise<void>(resolve => { release = resolve; });
        const originalSend = f.transport.send.bind(f.transport);
        f.transport.send = async (room, text, replyTo) => {
            if (replyTo === blocked.id) { signalStart(); await held; }
            return originalSend(room, text, replyTo);
        };
        let closeTask: Promise<void> | undefined;
        try {
            const input = message({ text: 'unfinished foreground task' });
            await f.gateway.message(input);
            await until(() => f.workers[0]?.sent.length === 1);
            const worker = f.workers[0]!;
            const session = worker.session.id;
            await f.gateway.message(blocked);
            await started;
            closeTask = f.gateway.close();
            const completed = await Promise.race([closeTask.then(() => true), delay(1200).then(() => false)]);
            assert.equal(completed, true, 'informational transport must not hold the service state lock at shutdown');
            assert.equal(worker.stopped, true, 'native worker cleanup must still run');
            assert.equal(f.store.session(worker.room.key).id, session, 'shutdown preserves the selected session');
            assert.notEqual(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'done');
            assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(blocked.id)?.status,
                kind === 'status' ? 'done' : 'cancelled');

            // The remote send may finish after close. That cannot touch session/output SQLite state.
            let lateOutputIndex = 0;
            f.store.recordOutput = () => { lateOutputIndex++; };
            release();
            await until(() => f.transport.sent.some(s => s.replyTo === blocked.id));
            await delay(20);
            assert.equal(lateOutputIndex, 0, 'late informational send cannot access a closed store');
        } finally {
            release?.();
            await closeTask?.catch(() => {});
            await f.close();
        }
    });
}
