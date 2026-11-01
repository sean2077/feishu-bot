import test from 'node:test';
import assert from 'node:assert/strict';
import { rm } from 'node:fs/promises';
import { Gateway } from '../src/gateway.ts';
import type { Delivery, Room, Session, WorkerEvent, WorkerFactory } from '../src/types.ts';
import { fixture, message, until } from './helpers.ts';

// Real gateway/SQLite, deliberately failing Worker boundary. No native process
// or external service is represented by these test doubles.
for (const initializing of [false, true]) {
    test(`service shutdown reports unconfirmed termination (${initializing ? 'initializing' : 'active'} worker)`, async () => {
        const f = await fixture();
        await f.gateway.close();
        const secret = 'fixture-shutdown-secret';
        let releaseStartup!: () => void;
        const startup = new Promise<void>(resolve => { releaseStartup = resolve; });
        const workers: { room: Room; session: Session; sent: Delivery[]; emit: (event: WorkerEvent) => void; stopCalls: number }[] = [];
        let cleanup = false;
        const factory: WorkerFactory = async (room, session, emit) => {
            const worker = { room, session, sent: [] as Delivery[], emit, stopCalls: 0 };
            workers.push(worker);
            if (initializing && room.chatId === 'group1') await startup;
            return {
                send: async delivery => { worker.sent.push(delivery); emit({ type: 'accepted', uuid: delivery.uuid }); },
                respond: () => {},
                stop: async () => {
                    worker.stopCalls++;
                    if (!cleanup && room.chatId === 'group1') throw new Error(`fixture stop failure: ${secret}`);
                }
            };
        };
        const gateway = new Gateway(f.config, f.store, f.transport, factory, [secret]);
        try {
            await gateway.message(message({ chatId: 'group2' }));
            await until(() => workers.some(w => w.room.chatId === 'group2' && w.sent.length === 1));
            await gateway.message(message());
            await until(() => workers.some(w => w.room.chatId === 'group1'));
            const failing = workers.find(w => w.room.chatId === 'group1')!;
            const sessionId = failing.session.id;
            if (!initializing) {
                await until(() => failing.sent.length === 1);
                failing.emit({ type: 'request', kind: 'permission', id: 'shutdown-permission', name: 'Bash', input: { command: 'fixture-only' } });
                await until(() => gateway.approvals.size(sessionId) === 1);
            }
            const closing = gateway.close();
            void closing.catch(() => {}); // Observe the rejection below, after releasing startup.
            releaseStartup();
            await assert.rejects(closing, error => {
                assert.ok(error instanceof Error);
                assert.match(error.message, /termination unconfirmed/i);
                assert.equal(error.message.includes(secret), false, 'public shutdown failure must not expose credentials');
                return true;
            });
            assert.ok(workers.every(w => w.stopCalls > 0), 'one failed worker must not skip another worker cleanup');
            assert.equal(gateway.approvals.size(sessionId), 0, 'shutdown still cancels pending approvals');
            assert.equal(f.store.session(failing.room.key).id, sessionId, 'shutdown failure must retain session identity');
            assert.equal(f.transport.receiptStates.some(r => r.state === 'done'), false, 'failed shutdown is never Done');
            const audit = JSON.stringify(f.store.db.prepare('SELECT event,detail FROM audit').all());
            assert.equal(audit.includes(secret), false, 'diagnostics must redact the worker error');
            if (initializing) assert.equal(failing.sent.length, 0, 'a late worker must not receive input after closing');
        } finally {
            cleanup = true; releaseStartup();
            await gateway.close().catch(() => {});
            f.store.close();
            await rm(f.dir, { recursive: true, force: true });
        }
    });
}
