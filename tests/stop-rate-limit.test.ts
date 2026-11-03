import test from 'node:test';
import assert from 'node:assert/strict';
import { fixture, message, until } from './helpers.ts';

test('a maintainer can stop the existing worker after exhausting the chat message rate', async () => {
    const f = await fixture();
    try {
        f.config.limits.messagesPerMinute = 1;
        await f.gateway.message(message({ senderId: 'admin', text: 'inspect this task' }));
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, sessionId = worker.session.id;

        const limited = message({ senderId: 'admin', text: 'another task' });
        await f.gateway.message(limited);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(limited.id)?.status, 'cancelled');
        assert.equal(worker.sent.length, 1, 'normal work must still be rate limited');

        await f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        assert.equal(worker.stopped, true, 'rate limiting must not make authorized stop unavailable');
        assert.equal(f.store.session(worker.room.key).id, sessionId, 'stop preserves the native session');
        assert.equal(f.workers.length, 1, 'stop never starts a second worker');
    } finally {
        await f.close();
    }
});

test('an authorized private-chat user can stop after rate exhaustion without opening a group control bypass', async () => {
    const f = await fixture();
    try {
        f.config.limits.messagesPerMinute = 1;
        await f.gateway.message(message({ chatType: 'p2p', chatId: 'dm-a', senderId: 'alice',
            mentioned: false, text: 'inspect this task' }));
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;

        await f.gateway.message(message({ chatType: 'p2p', chatId: 'dm-a', senderId: 'alice',
            mentioned: false, text: '/stop' }));
        assert.equal(worker.stopped, true, 'a private-chat owner can stop without another normal-message allowance');

        await f.gateway.message(message({ senderId: 'bob', text: 'start group work' }));
        await until(() => f.workers[1]?.sent.length === 1);
        const groupWorker = f.workers[1]!;
        await f.gateway.message(message({ senderId: 'bob', text: '/stop' }));
        assert.equal(groupWorker.stopped, false, 'a non-maintainer cannot stop a shared group');
    } finally {
        await f.close();
    }
});
