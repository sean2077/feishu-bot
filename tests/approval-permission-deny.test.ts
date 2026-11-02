import test from 'node:test';
import assert from 'node:assert/strict';
import { fixture, message, until } from './helpers.ts';
import { route } from '../src/policy.ts';

test('a permitted group member cannot reject a maintainer-only tool permission', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        const session = f.store.session(room.key);
        const pending = f.gateway.approvals.request(room, session.id, 'Bash', { command: 'fixture-only' }, 'Test permission');
        await until(() => f.transport.sent.some(sent => !!sent.card));
        const cardId = f.transport.sent.find(sent => !!sent.card)!.id;
        const requestId = String(f.store.db.prepare("SELECT id FROM requests WHERE session=? AND state='pending'").get(session.id)!.id);

        // Alice is an allowed group participant, but not a project maintainer.
        // Both approval and rejection are permission decisions, not group polls.
        assert.equal(await f.gateway.approvals.click(room.chatId, 'alice',
            { request: requestId, decision: 'deny' }, cardId), '仅维护者可以审批');
        assert.equal(await f.gateway.approvals.click(room.chatId, 'alice',
            { request: requestId, decision: 'allow' }, cardId), '仅维护者可以审批');
        assert.equal(f.store.db.prepare('SELECT state FROM requests WHERE id=?').get(requestId)!.state, 'pending');
        assert.equal(f.gateway.approvals.size(session.id), 1);
        assert.equal(f.transport.updates.length, 0, 'the pending card remains actionable by maintainers');

        assert.equal(await f.gateway.approvals.click(room.chatId, 'admin',
            { request: requestId, decision: 'deny' }, cardId), '已拒绝');
        assert.deepEqual(await pending, { approved: false, reason: 'denied' });
        assert.equal(f.store.db.prepare('SELECT state,actor FROM requests WHERE id=?').get(requestId)!.state, 'denied');
        assert.equal(f.gateway.approvals.size(session.id), 0);
        assert.equal(await f.gateway.approvals.click(room.chatId, 'admin',
            { request: requestId, decision: 'allow' }, cardId), '请求已结束或过期');
    } finally { await f.close(); }
});

test('ordinary authorized group members can still dismiss a shared question', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        const session = f.store.session(room.key);
        const pending = f.gateway.approvals.request(room, session.id, 'AskUserQuestion', {}, 'Test question', undefined,
            [{ question: 'Proceed?', options: [{ label: 'Yes' }, { label: 'No' }] }]);
        await until(() => f.transport.sent.some(sent => !!sent.card));
        const cardId = f.transport.sent.find(sent => !!sent.card)!.id;
        const requestId = String(f.store.db.prepare("SELECT id FROM requests WHERE session=? AND state='pending'").get(session.id)!.id);
        assert.equal(await f.gateway.approvals.click(room.chatId, 'alice',
            { request: requestId, decision: 'deny' }, cardId), '已拒绝');
        assert.deepEqual(await pending, { approved: false, reason: 'denied' });
    } finally { await f.close(); }
});
