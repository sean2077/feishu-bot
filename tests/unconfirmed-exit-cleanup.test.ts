import test from 'node:test';
import assert from 'node:assert/strict';
import { rm } from 'node:fs/promises';
import { Gateway } from '../src/gateway.ts';
import type { Delivery, Room, Session, WorkerEvent, WorkerFactory } from '../src/types.ts';
import { fixture, message, until } from './helpers.ts';

// Real gateway and SQLite; a failed descendant cleanup is injected at the
// Worker event boundary. It is not an actual OS-process or Feishu failure.
test('failed cleanup after an unexpected exit reserves the original session owner', async () => {
    const f = await fixture();
    await f.gateway.close();
    f.config.runtime.maxWorkers = 1;
    const workers: { room: Room; session: Session; sent: Delivery[]; emit: (event: WorkerEvent) => void }[] = [];
    const factory: WorkerFactory = async (room, session, emit) => {
        const worker = { room, session, sent: [] as Delivery[], emit };
        workers.push(worker);
        return {
            send: async delivery => { worker.sent.push(delivery); emit({ type: 'accepted', uuid: delivery.uuid }); },
            respond: () => {},
            stop: async () => { throw new Error('fixture: cleanup still unconfirmed'); }
        };
    };
    const gateway = new Gateway(f.config, f.store, f.transport, factory);
    try {
        await gateway.message(message());
        await until(() => workers[0]?.sent.length === 1);
        const first = workers[0]!, sessionId = first.session.id;
        first.emit({ type: 'request', kind: 'permission', id: 'fixture-permission', name: 'Bash', input: { command: 'fixture-only' } });
        await until(() => gateway.approvals.size(sessionId) === 1);
        first.emit({ type: 'exit', code: 17, cleanupConfirmed: false });
        await until(() => gateway.approvals.size(sessionId) === 0);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE uuid=?').get(first.sent[0]!.uuid)?.status, 'uncertain',
            'an interrupted native input must not become Done or be replayed');
        await gateway.message(message({ chatId: 'group2' }));
        await until(() => workers.length > 1 || f.transport.sent.some(s => s.text?.includes('输入已保存')));
        assert.equal(workers.length, 1, 'unconfirmed descendants still occupy the original capacity');
        await gateway.message(message({ senderId: 'admin', text: '/status' }));
        assert.match(f.transport.sent.at(-1)?.text ?? '', /termination_unconfirmed/);
        await gateway.message(message({ senderId: 'admin', text: '/new' }));
        assert.equal(f.store.session(first.room.key).id, sessionId, 'a failed stop must not rotate an unconfirmed transcript');
        await assert.rejects(gateway.close(), /termination unconfirmed/i);
        const alert = f.store.db.prepare("SELECT state,text FROM outbox WHERE room=? AND kind='recovery'").get(first.room.key);
        assert.equal(alert?.state, 'done', 'unconfirmed descendant cleanup must leave a durable warning');
        assert.match(String(alert?.text), /后代清理未确认/);
    } finally {
        await gateway.close().catch(() => {});
        f.store.close();
        await rm(f.dir, { recursive: true, force: true });
    }
});

test('worker initialization failure with unconfirmed cleanup does not free capacity', async () => {
    const f = await fixture();
    await f.gateway.close();
    f.config.runtime.maxWorkers = 1;
    let attempts = 0;
    const factory: WorkerFactory = async (_room, _session, emit) => {
        attempts++;
        emit({ type: 'exit', code: null, cleanupConfirmed: false });
        throw new Error('fixture: initialization cleanup failed');
    };
    const gateway = new Gateway(f.config, f.store, f.transport, factory);
    try {
        await gateway.message(message());
        await until(() => attempts === 1);
        const sessionBefore = f.store.db.prepare("SELECT id FROM sessions WHERE state='open'").get()?.id;
        await gateway.message(message({ senderId: 'admin', text: '/status' }));
        assert.match(f.transport.sent.at(-1)?.text ?? '', /termination_unconfirmed/);
        await gateway.message(message({ senderId: 'admin', text: '/new' }));
        assert.equal(f.store.db.prepare("SELECT id FROM sessions WHERE state='open'").get()?.id, sessionBefore,
            'no replacement session can be created after unconfirmed startup cleanup');
        await gateway.message(message({ chatId: 'group2' }));
        await until(() => attempts > 1 || f.transport.sent.some(s => s.text?.includes('输入已保存')));
        assert.equal(attempts, 1, 'other rooms cannot reuse capacity while a child might survive');
        await assert.rejects(gateway.close(), /termination unconfirmed/i);
    } finally {
        await gateway.close().catch(() => {});
        f.store.close();
        await rm(f.dir, { recursive: true, force: true });
    }
});
