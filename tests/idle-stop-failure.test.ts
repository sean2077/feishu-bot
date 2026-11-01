import test from 'node:test';
import assert from 'node:assert/strict';
import { rm } from 'node:fs/promises';
import { Gateway } from '../src/gateway.ts';
import type { Delivery, Room, Session, WorkerEvent, WorkerFactory } from '../src/types.ts';
import { fixture, message, until } from './helpers.ts';

// The fake stop deliberately rejects without terminating anything. A failed
// stop must not allow another worker to occupy its slot or reuse its session.
test('failed idle stop retains capacity and refuses a second native session owner', async () => {
    const f = await fixture();
    await f.gateway.close();
    f.config.runtime.maxWorkers = 1;
    f.config.runtime.idleMs = 0;
    const workers: { room: Room; session: Session; sent: Delivery[]; emit: (event: WorkerEvent) => void }[] = [];
    let stopCalls = 0;
    const factory: WorkerFactory = async (room, session, emit) => {
        const worker = { room, session, sent: [] as Delivery[], emit };
        workers.push(worker);
        return {
            send: async delivery => { worker.sent.push(delivery); emit({ type: 'accepted', uuid: delivery.uuid }); },
            respond: () => {},
            stop: async () => { stopCalls++; throw new Error('fixture: termination not confirmed'); }
        };
    };
    const gateway = new Gateway(f.config, f.store, f.transport, factory);
    try {
        await gateway.message(message());
        await until(() => workers[0]?.sent.length === 1);
        const first = workers[0]!, sessionId = first.session.id;
        first.emit({ type: 'activity', state: 'idle', background: 0, pinned: false });
        first.emit({ type: 'result', text: 'completed', success: true, uuids: [first.sent[0]!.uuid] });
        await until(() => f.store.countPending(first.room.key) === 0);
        await until(() => f.transport.sent.some(s => s.card !== undefined));

        await gateway.message(message({ chatId: 'group2' }));
        await until(() => stopCalls > 0);
        // A second admission must not turn the earlier stop error into free capacity.
        await gateway.message(message({ chatId: 'group2' }));
        await until(() => workers.length > 1 || f.transport.sent.some(s => s.text?.includes('当前会话')));
        assert.equal(workers.length, 1, 'unconfirmed stop must reserve the original process slot');

        await gateway.message(message({ senderId: 'admin', text: '/status' }));
        assert.match(f.transport.sent.at(-1)?.text ?? '', /termination_unconfirmed/);
        await gateway.message(message({ senderId: 'admin', text: '/new' }));
        assert.equal(f.store.session(first.room.key).id, sessionId, 'failed stop cannot rotate a live transcript');
        assert.match(f.transport.sent.at(-1)?.text ?? '', /未确认退出/);
    } finally {
        await assert.rejects(gateway.close(), /termination unconfirmed/i);
        f.store.close();
        await rm(f.dir, { recursive: true, force: true });
    }
});
