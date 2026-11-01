import test from 'node:test';
import assert from 'node:assert/strict';
import { Gateway, route } from '../src/gateway.ts';
import { fixture, message, until } from './helpers.ts';

test('unexpected native exit persists a notification when Feishu create outcome is unknown', async () => {
    const f = await fixture();
    const originalCard = f.transport.card.bind(f.transport);
    const originalSend = f.transport.send.bind(f.transport);
    try {
        const input = message();
        const room = route(f.config, input)!;
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const sessionId = worker.session.id;

        worker.emit({ type: 'request', kind: 'permission', id: 'fixture-approval', name: 'Bash',
            input: { command: 'fixture-only' } });
        await until(() => f.gateway.approvals.size(sessionId) === 1 &&
            f.transport.sent.some(entry => entry.card !== undefined));

        let creates = 0;
        const ambiguous = async (): Promise<string> => {
            creates++;
            throw new Error('fixture: response lost after remote acceptance');
        };
        f.transport.card = ambiguous;
        f.transport.send = ambiguous;
        worker.emit({ type: 'exit', code: 17 });
        await until(() => f.store.db.prepare("SELECT state FROM outbox WHERE kind='recovery'").get()?.state === 'uncertain');

        const pending = f.store.db.prepare("SELECT id,room,session,state,text,kind FROM outbox WHERE kind='recovery'").all();
        assert.equal(pending.length, 1, 'an unexpected exit warning must survive a failed external create');
        assert.equal(pending[0]!.room, room.key);
        assert.equal(pending[0]!.session, sessionId);
        assert.match(String(pending[0]!.text), /意外退出/);
        assert.equal(creates, 1);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'uncertain');
        assert.equal(f.store.db.prepare("SELECT state FROM requests WHERE session=?").get(sessionId)?.state, 'denied');
        assert.equal(f.store.session(room.key).id, sessionId);

        await f.gateway.close();
        const restarted = new Gateway(f.config, f.store, f.transport, f.factory);
        try {
            await restarted.recover();
            await restarted.sweep();
            assert.equal(creates, 1, 'restart and housekeeping must not repeat an unknown Feishu write');
            assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM outbox WHERE kind='recovery'").get()?.n, 1);
            assert.equal(f.workers.length, 1, 'recovery must not replay an interrupted request');
            f.transport.card = originalCard;
            f.transport.send = originalSend;
            assert.equal(await restarted.outbox.deliver(String(pending[0]!.id), room, true), true);
            assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(pending[0]!.id)?.state, 'done');
        } finally {
            await restarted.close();
        }
    } finally {
        f.transport.card = originalCard;
        f.transport.send = originalSend;
        await f.close();
    }
});

test('exit state changes roll back when its durable notification cannot be saved', async () => {
    const f = await fixture();
    const save = f.gateway.outbox.save.bind(f.gateway.outbox);
    try {
        const input = message();
        const room = route(f.config, input)!;
        await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        worker.emit({ type: 'request', kind: 'permission', id: 'fixture-approval', name: 'Bash',
            input: { command: 'fixture-only' } });
        await until(() => f.gateway.approvals.size(worker.session.id) === 1);
        f.gateway.outbox.save = () => { throw new Error('fixture: durable notice insert failed'); };
        assert.throws(() => worker.emit({ type: 'exit', code: 17 }), /durable notice insert failed/);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'sent');
        assert.equal(f.store.db.prepare("SELECT state FROM requests WHERE session=?").get(worker.session.id)?.state, 'pending');
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM outbox').get()?.n, 0);
        f.gateway.outbox.save = save;

        await f.gateway.close();
        const restarted = new Gateway(f.config, f.store, f.transport, f.factory);
        try {
            await restarted.recover();
            assert.equal(f.store.db.prepare("SELECT state FROM requests WHERE session=?").get(worker.session.id)?.state, 'expired');
            const notices = f.store.db.prepare("SELECT kind,state,session FROM outbox WHERE room=?").all(room.key);
            assert.equal(notices.length, 1);
            assert.equal(notices[0]!.kind, 'recovery');
            assert.equal(notices[0]!.session, worker.session.id);
            assert.equal(notices[0]!.state, 'done');
        } finally {
            await restarted.close();
        }
    } finally {
        f.gateway.outbox.save = save;
        await f.close();
    }
});
