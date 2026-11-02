import test from 'node:test';
import assert from 'node:assert/strict';
import { rm } from 'node:fs/promises';
import { fixture, message, until } from './helpers.ts';
import { route } from '../src/policy.ts';

test('a late approval card is visibly closed after Gateway and SQLite shut down', async () => {
    const f = await fixture();
    const room = route(f.config, message())!;
    const session = f.store.session(room.key);
    let resolveCard: ((id: string) => void) | undefined;
    let gatewayClosed = false, storeClosed = false;
    try {
        // Reproduce a remote card create that succeeds only after local shutdown.
        f.transport.card = async () => new Promise<string>(resolve => { resolveCard = resolve; });
        const waiting = f.gateway.approvals.request(room, session.id, 'Bash', {}, 'test-only approval');
        await until(() => !!resolveCard);
        await f.gateway.close();
        gatewayClosed = true;
        assert.deepEqual(await waiting, { approved: false, reason: 'shutdown' });
        assert.equal(f.store.db.prepare("SELECT state FROM requests").get()!.state, 'denied');

        f.store.close();
        storeClosed = true;
        resolveCard!('late-approval-card');
        // The old implementation attempted store.recordOutput() first, which
        // threw on closed SQLite and skipped the best-effort disabling update.
        await until(() => f.transport.updates.some(x => x.id === 'late-approval-card'));
        const card = f.transport.updates.find(x => x.id === 'late-approval-card')!.card;
        assert.ok(JSON.stringify(card).includes('请求已结束'));
        assert.ok(!JSON.stringify(card).includes('批准一次'));
        assert.equal(f.transport.updates.filter(x => x.id === 'late-approval-card').length, 1);
    } finally {
        resolveCard?.('late-approval-card');
        if (!gatewayClosed) await f.gateway.close();
        if (!storeClosed) f.store.close();
        await rm(f.dir, { recursive: true, force: true });
    }
});

test('a late approval card after /stop cannot become a registered output or active callback', async () => {
    const f = await fixture();
    let resolveCard: ((id: string) => void) | undefined;
    try {
        const room = route(f.config, message())!;
        const session = f.store.session(room.key);
        f.transport.card = async () => new Promise<string>(resolve => { resolveCard = resolve; });
        const waiting = f.gateway.approvals.request(room, session.id, 'Bash', {}, 'test-only approval');
        await until(() => !!resolveCard);
        const requestId = String(f.store.db.prepare("SELECT id FROM requests WHERE session=?").get(session.id)!.id);

        await f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        assert.deepEqual(await waiting, { approved: false, reason: 'cancelled' });
        resolveCard!('late-stopped-approval');
        await until(() => f.transport.updates.some(x => x.id === 'late-stopped-approval'));
        assert.equal(f.store.db.prepare('SELECT id FROM outputs WHERE id=?').get('late-stopped-approval'), undefined,
            'a settled request must not index a newly created interactive card');
        assert.equal(await f.gateway.approvals.click(room.chatId, 'admin', { request: requestId, decision: 'allow' }, 'late-stopped-approval'),
            '请求已结束或过期');
        assert.equal(f.store.db.prepare('SELECT state FROM requests WHERE id=?').get(requestId)!.state, 'denied');
    } finally {
        resolveCard?.('late-stopped-approval');
        await f.close();
    }
});
