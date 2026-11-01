import test from 'node:test';
import assert from 'node:assert/strict';
import { Gateway, route } from '../src/gateway.ts';
import { fixture, message, until } from './helpers.ts';

test('a native result does not complete its input before the final answer is durable', async () => {
    const f = await fixture();
    const originalSave = f.gateway.outbox.save.bind(f.gateway.outbox);
    try {
        const input = message();
        const room = route(f.config, input)!;
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const uuid = worker.sent[0]!.uuid;
        const sessionId = worker.session.id;
        worker.emit({ type: 'consumed', uuid });
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'consumed');

        // Simulate a failed local persistence operation before any Feishu create.
        // A crash here must leave the input eligible for recovery notification.
        f.gateway.outbox.save = () => { throw new Error('fixture: final answer was not persisted'); };
        worker.emit({ type: 'result', uuid, success: true, text: 'completed model output' });
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'consumed');
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM outbox').get()?.n, 0);
        f.gateway.outbox.save = originalSave;

        await f.gateway.close();
        const restarted = new Gateway(f.config, f.store, f.transport, f.factory);
        try {
            await restarted.recover();
            const warning = f.store.db.prepare("SELECT kind,state,session,text FROM outbox WHERE kind='recovery'").get();
            assert.equal(warning?.kind, 'recovery');
            assert.equal(warning?.state, 'done');
            assert.equal(warning?.session, sessionId);
            assert.match(String(warning?.text), /未确认完成/);
            assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'uncertain');
            assert.equal(f.workers.length, 1, 'an unpersisted result must not replay model work');
            assert.equal(f.transport.receiptStates.some(row => row.id === input.id && row.state === 'done'), false);
        } finally {
            await restarted.close();
        }
    } finally {
        f.gateway.outbox.save = originalSave;
        await f.close();
    }
});

test('a persisted final answer completes the input once without additional model work', async () => {
    const f = await fixture();
    try {
        const input = message();
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const uuid = worker.sent[0]!.uuid;
        worker.emit({ type: 'consumed', uuid });
        worker.emit({ type: 'result', uuid, success: true, text: 'persisted answer' });
        await until(() => f.store.db.prepare("SELECT state FROM outbox WHERE kind='reply'").get()?.state === 'done');
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'done');
        assert.equal(f.store.db.prepare("SELECT text FROM outbox WHERE kind='reply'").get()?.text, 'persisted answer');
        assert.equal(f.workers.length, 1);
    } finally {
        await f.close();
    }
});
