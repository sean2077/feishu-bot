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

test('failure completing the input rolls back the already-staged final outbox row', async () => {
    const f = await fixture();
    const originalMark = f.store.mark.bind(f.store);
    try {
        const input = message();
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const uuid = worker.sent[0]!.uuid;
        worker.emit({ type: 'consumed', uuid });
        f.store.mark = (id, status) => {
            if (status === 'done') throw new Error('fixture: cannot complete originating input');
            return originalMark(id, status);
        };
        worker.emit({ type: 'result', uuid, success: true, text: 'staged but uncommitted' });
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'consumed');
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM outbox').get()?.n, 0,
            'the answer row and input completion must commit or roll back together');
        f.store.mark = originalMark;
    } finally {
        f.store.mark = originalMark;
        await f.close();
    }
});

test('shutdown after final persistence preserves an in-flight preview result for recovery', async () => {
    const f = await fixture();
    let releasePreview: (() => void) | undefined;
    let previewStarted = false;
    let creates = 0;
    const preview = new Promise<string>(resolve => { releasePreview = () => resolve('preview-card'); });
    f.transport.card = async () => { creates++; previewStarted = true; return preview; };
    try {
        const input = message();
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const uuid = worker.sent[0]!.uuid;
        worker.emit({ type: 'consumed', uuid });
        worker.emit({ type: 'text', turnId: uuid, text: 'partial preview', partial: false });
        await until(() => previewStarted);
        worker.emit({ type: 'result', uuid, success: true, text: 'durable final answer' });
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'done');
        assert.equal(f.store.db.prepare("SELECT state FROM outbox WHERE kind='reply'").get()?.state, 'preparing');

        // Service shutdown aborts the preview renderer while it is waiting for
        // a Feishu create response. The final answer has already committed.
        const closing = f.gateway.close();
        releasePreview!();
        await closing;
        const row = f.store.db.prepare("SELECT id,state,target,text FROM outbox WHERE kind='reply'").get()!;
        assert.equal(row.text, 'durable final answer');
        assert.equal(row.state, 'pending', 'shutdown must not discard a committed final answer');
        assert.equal(row.target, 'preview-card', 'the known remote ID must survive shutdown');
        assert.equal(creates, 1);

        const restarted = new Gateway(f.config, f.store, f.transport, f.factory);
        try {
            await restarted.recover();
            await restarted.sweep();
            assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(row.id)?.state, 'done');
            assert.equal(creates, 1, 'recovery must update the known card instead of creating another');
            assert.ok(f.transport.updates.some(update => update.id === 'preview-card'));
        } finally {
            await restarted.close();
        }
    } finally {
        releasePreview?.();
        await f.close();
    }
});
