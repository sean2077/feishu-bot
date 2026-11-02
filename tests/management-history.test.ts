import test from 'node:test';
import assert from 'node:assert/strict';
import { fixture, message } from './helpers.ts';
import { route } from '../src/policy.ts';

test('an owned old session can be selected and stopped without losing durable outputs behind the list limit', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        let oldest = '';
        for (let i = 0; i < 22; i++) {
            const s = f.store.newSession(room.key);
            f.store.db.prepare('UPDATE sessions SET created=? WHERE id=?').run(1000 + i, s.id);
            if (!oldest) oldest = s.id;
        }
        const resume = message({ senderId: 'admin', text: `/resume ${oldest}` });
        await f.gateway.message(resume);
        assert.match(f.transport.sent.find(item => item.replyTo === resume.id)!.text!, /已选择历史会话/);
        assert.equal(f.store.session(room.key).id, oldest);
        const sessions = f.store.listSessions(room.key);
        assert.equal(sessions.length, 21);
        assert.equal(sessions.filter(s => s.state === 'open').length, 1);
        assert.ok(sessions.some(s => s.id === oldest));
        const output = f.gateway.outbox.save(room, oldest, 'pending answer', true);
        const other = route(f.config, message({ chatId: 'group2' }))!;
        const foreign = f.store.newSession(other.key);
        await f.gateway.message(message({ senderId: 'admin', text: `/resume ${foreign.id}` }));
        assert.equal(f.store.session(room.key).id, oldest);
        assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(output)!.state, 'pending');
        await f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(output)!.state, 'cancelled');
        assert.equal(f.store.session(room.key).id, oldest);
        assert.equal(f.workers.length, 0);
    } finally { await f.close(); }
});

test('management hints recognize a private conversation owner without granting tool approval', async () => {
    const f = await fixture();
    try {
        const input = message({ chatId: 'dm-alice', chatType: 'p2p', text: '/help', mentioned: false });
        await f.gateway.message(input);
        const text = f.transport.sent.find(item => item.replyTo === input.id)!.text!;
        assert.match(text, /你可以管理本聊天会话/);
        assert.match(text, /工具审批仍限项目维护者/);
        assert.equal(f.workers.length, 0);
    } finally { await f.close(); }
});
