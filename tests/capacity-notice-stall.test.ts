import test from 'node:test';
import assert from 'node:assert/strict';
import { fixture, message, until } from './helpers.ts';

test('a stalled capacity notice must not prevent dispatch after another room releases a worker', { timeout: 10000 }, async () => {
    const f = await fixture();
    f.config.runtime.maxWorkers = 1;
    const originalSend = f.transport.send.bind(f.transport);
    let releaseNotice!: () => void;
    const stalled = new Promise<void>(resolve => { releaseNotice = resolve; });
    let noticeStarted = false;
    f.transport.send = async (room, text, replyTo, threadId) => {
        if (room.chatId === 'group2' && text.includes('当前会话都在工作')) {
            noticeStarted = true;
            await stalled;
        }
        return originalSend(room, text, replyTo, threadId);
    };
    try {
        const first = message({ chatId: 'group1', text: 'hold the only active worker' });
        await f.gateway.message(first);
        await until(() => f.workers[0]?.sent.length === 1);
        const waiting = message({ chatId: 'group2', text: 'must run when capacity returns' });
        await f.gateway.message(waiting);
        await until(() => noticeStarted);
        assert.equal(f.workers.length, 1, 'capacity is still occupied');

        await f.gateway.message(message({ chatId: 'group1', senderId: 'admin', text: '/stop' }));
        assert.equal(f.workers[0]!.stopped, true);
        // The informational Feishu send is deliberately still unresolved.
        // Capacity recovery must not depend on that unrelated network response.
        await until(() => f.workers.some(w => w.room.chatId === 'group2' && w.sent.length === 1), 1200);
        const resumed = f.workers.find(w => w.room.chatId === 'group2')!;
        assert.equal(resumed.sent[0]!.message.id, waiting.id);
        assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM inbox WHERE id=?").get(waiting.id)!.n, 1);
    } finally {
        releaseNotice();
        await f.close();
    }
});
