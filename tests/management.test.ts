import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { fixture, message, until } from './helpers.ts';
import { route } from '../src/policy.ts';

async function read(f: Awaited<ReturnType<typeof fixture>>, text: string, senderId = 'alice') {
    const input = message({ text, senderId });
    await f.gateway.message(input);
    const response = f.transport.sent.find(item => item.replyTo === input.id);
    assert.ok(response?.text, `missing response for ${text}`);
    return response.text;
}

test('management reads are understandable without starting a worker or creating a conversation', async () => {
    const f = await fixture();
    try {
        assert.match(await read(f, '/status'), /尚未启动/);
        const status = JSON.parse(await read(f, '/status --json'));
        assert.equal(status.state, 'not_started');
        assert.deepEqual(status.inputs, { queued: 0, sent: 0, consumed: 0, uncertain: 0 });
        assert.equal(status.pendingOutputs, 0);
        assert.match(await read(f, '/sessions'), /尚无历史会话/);
        assert.match(await read(f, '/outputs'), /没有待交付/);
        const help = await read(f, '/help');
        assert.match(help, /\/outputs/);
        assert.match(help, /\/retry-output/);
        assert.match(help, /维护者/);
        assert.match(await read(f, '/status --typo'), /用法/);
        assert.equal(f.workers.length, 0);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM sessions').get()!.n, 0);
        assert.equal(f.transport.receipts.length, 0);
    } finally { await f.close(); }
});

test('status separates queued and uncertain inputs without reading another room or replaying work', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        f.store.insert(room.key, message({ text: 'PRIVATE_INPUT_BODY' }), 'uncertain');
        f.store.insert(room.key, message(), 'queued');
        const foreign = message({ chatId: 'group2' });
        f.store.insert(route(f.config, foreign)!.key, foreign, 'uncertain');
        const status = JSON.parse(await read(f, '/status --json'));
        assert.deepEqual(status.inputs, { queued: 1, sent: 0, consumed: 0, uncertain: 1 });
        const text = await read(f, '/status');
        assert.match(text, /排队：1/);
        assert.match(text, /结果未知：1/);
        assert.match(text, /不会自动重放/);
        assert.ok(!text.includes('PRIVATE_INPUT_BODY'));
        assert.equal(f.workers.length, 0);
        assert.equal(f.store.countPending(room.key), 1);
    } finally { await f.close(); }
});

test('status explains unknown background activity, questions and suspended native sessions', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, nativeId = randomUUID();
        w.emit({ type: 'ready', ccId: nativeId });
        assert.match(await read(f, '/status'), /处理中/);
        w.emit({ type: 'result', text: 'answer', success: true, uuid: w.sent[0]!.uuid });
        await until(() => f.transport.receiptStates.some(item => item.state === 'done'));
        w.emit({ type: 'activity', state: 'idle', background: -1 });
        assert.match(await read(f, '/status'), /后台活动：未知/);
        w.emit({ type: 'request', kind: 'question', id: 'question', name: 'AskUserQuestion', input: {
            questions: [{ question: 'Choose?', options: [{ label: 'yes' }, { label: 'no' }] }]
        } });
        await until(() => f.gateway.approvals.size(w.session.id) === 1);
        assert.match(await read(f, '/status'), /等待审批或回答/);
        assert.match(await read(f, '/new'), /维护者/);
        assert.equal(w.stopped, false);
        await read(f, '/stop', 'admin');
        const status = JSON.parse(await read(f, '/status --json'));
        assert.equal(status.state, 'suspended');
        assert.equal(status.session, w.session.id);
        assert.equal(status.nativeSessionId, nativeId);
        assert.match(await read(f, '/status'), /下条消息继续原会话/);
        assert.equal(f.workers.length, 1);
    } finally { await f.close(); }
});

test('outputs explain recovery by state and role without exposing answer bodies or file paths', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!, session = f.store.session(room.key);
        const pending = f.gateway.outbox.save(room, session.id, 'PRIVATE_ANSWER_BODY', true, 'known-card');
        const uncertain = f.gateway.outbox.save(room, session.id, 'PRIVATE_OTHER_BODY', true);
        f.store.db.prepare("UPDATE outbox SET state='uncertain',file_state='uncertain',file_path=? WHERE id=?")
            .run('/private/fixture-path', uncertain);
        const other = route(f.config, message({ chatId: 'group2' }))!;
        const foreign = f.gateway.outbox.save(other, f.store.session(other.key).id, 'FOREIGN_BODY', true);
        const member = await read(f, '/outputs');
        assert.match(member, /待交付输出/);
        assert.match(member, /结果未知/);
        assert.match(member, /可能重复/);
        assert.match(member, /维护者/);
        assert.ok(member.includes(pending) && member.includes(uncertain));
        for (const secret of ['PRIVATE_ANSWER_BODY', 'PRIVATE_OTHER_BODY', 'fixture-path', foreign]) assert.ok(!member.includes(secret));
        assert.ok(!(await read(f, `/retry-output ${uncertain}`)).includes('已补发'));
        const admin = await read(f, '/outputs', 'admin');
        assert.ok(admin.includes(`/retry-output ${uncertain}`));
        const rows = JSON.parse(await read(f, '/outputs --json'));
        assert.equal(rows.length, 2);
        assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(uncertain)!.state, 'uncertain');
        assert.equal(f.workers.length, 0);
        assert.equal(f.transport.updates.length, 0);
    } finally { await f.close(); }
});

test('session list marks the selected conversation and discloses the recent-history limit', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        let oldest = '';
        for (let i = 0; i < 22; i++) {
            const session = f.store.newSession(room.key);
            f.store.db.prepare('UPDATE sessions SET created=? WHERE id=?').run(1000 + i, session.id);
            if (!oldest) oldest = session.id;
        }
        f.store.resume(room.key, oldest);
        const text = await read(f, '/sessions', 'admin');
        assert.match(text, /最近 20/);
        assert.match(text, /共 22/);
        assert.ok(text.includes(oldest), 'selected old session must not disappear behind the recent list limit');
        assert.match(text, /当前会话/);
        const status = JSON.parse(await read(f, '/status --json'));
        assert.equal(status.session, oldest);
        assert.equal(f.workers.length, 0);
    } finally { await f.close(); }
});

test('unauthorized and unmentioned management commands stay silent and outside storage', async () => {
    const f = await fixture();
    try {
        for (const overrides of [{ senderId: 'stranger' }, { chatId: 'foreign' }, { mentioned: false }]) {
            await f.gateway.message(message({ text: '/status', ...overrides }));
            await f.gateway.message(message({ text: '/outputs', ...overrides }));
        }
        assert.equal(f.transport.sent.length, 0);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n, 0);
        assert.equal(f.workers.length, 0);
    } finally { await f.close(); }
});
