import test from 'node:test';
import assert from 'node:assert/strict';
import { setTimeout as delay } from 'node:timers/promises';
import { fixture, message, until } from './helpers.ts';

/** An outbound notice must not block the same conversation's stop command. */
for (const kind of ['status', 'invalid input'] as const) {
    test(`a stalled ${kind} notice does not block /stop or an unrelated room`, { timeout: 10000 }, async () => {
        const f = await fixture();
        let started!: () => void;
        let release!: () => void;
        const attempted = new Promise<void>(resolve => { started = resolve; });
        const held = new Promise<void>(resolve => { release = resolve; });
        const originalSend = f.transport.send.bind(f.transport);
        const blocked = kind === 'status'
            ? message({ text: '/status' })
            : message({ text: 'unreadable resource', inputIssue: '附件无法完整解析' });
        f.transport.send = async (room, text, replyTo, threadId) => {
            if (replyTo === blocked.id) { started(); await held; }
            return originalSend(room, text, replyTo, threadId);
        };
        let pending!: Promise<void>;
        try {
            const initial = message({ text: 'long-running work' });
            await f.gateway.message(initial);
            await until(() => f.workers[0]?.sent.length === 1);
            const session = f.workers[0]!.session.id;
            pending = f.gateway.message(blocked);
            await attempted; // The simulated Feishu send will remain unresolved.
            const other = message({ chatId: 'group2', text: '/status' });
            await f.gateway.message(other);
            assert.ok(f.transport.sent.some(s => s.replyTo === other.id));
            const stopInput = message({ senderId: 'admin', text: '/stop' });
            const stop = f.gateway.message(stopInput);
            const finished = await Promise.race([stop.then(() => true), delay(1200).then(() => false)]);
            assert.equal(finished, true, '/stop should not wait for a stalled informational send');
            assert.equal(f.workers[0]!.stopped, true);
            assert.equal(f.workers[0]!.session.id, session);
            assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(initial.id)?.status, 'cancelled');
            assert.ok(f.transport.sent.some(s => s.replyTo === stopInput.id));
            release();
            await pending;
            await until(() => f.transport.sent.some(s => s.replyTo === blocked.id));
            if (kind === 'status') {
                assert.match(f.transport.sent.find(s => s.replyTo === blocked.id)!.text!, /处理中/);
                assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(blocked.id)?.status, 'done');
            } else {
                assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(blocked.id)?.status, 'cancelled');
            }
            assert.equal(f.workers.length, 1);
        } finally {
            release?.();
            await pending?.catch(() => {});
            await f.close();
        }
    });
}
