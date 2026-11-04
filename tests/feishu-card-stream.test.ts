import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { LarkChannel } from '@larksuite/channel';
import { Store } from '../src/store.ts';
import { createReplyCardStream } from '../src/feishu-card-stream.ts';
import { replyCard } from '../src/reply-card.ts';
import { displayCard } from '../src/approvals.ts';

const room = { key: 'group:oc_fixture', chatId: 'oc_fixture', projectId: 'fixture', maintainers: ['ou_fixture'] };
const card = (answer = '', thinking = 'Thinking', streaming = true) => ({ schema: '2.0', config: { streaming_mode: streaming, update_multi: true },
    body: { elements: [{ tag: 'markdown', element_id: 'reply_thinking', content: thinking }, { tag: 'markdown', element_id: 'reply_answer', content: answer }] } });
async function fixture(t: import('node:test').TestContext) {
    const dir = await mkdtemp(join(tmpdir(), 'card-stream-')), path = join(dir, 'state.sqlite');
    let store = new Store(path);
    store.db.exec('CREATE TABLE IF NOT EXISTS feishu_cards(message TEXT PRIMARY KEY,card TEXT NOT NULL,room TEXT NOT NULL,sequence INTEGER NOT NULL DEFAULT 0,snapshot TEXT NOT NULL)');
    const calls: { kind: string; input: any }[] = [];
    let failure: unknown, sendFailure: unknown, responseCode = 0, active = 0, maxActive = 0;
    let held: { kind: string; entered(): void; wait: Promise<void> } | undefined;
    const waitHeld = async (kind: string) => { if (held?.kind === kind) { held.entered(); await held.wait; } };
    const request = async (kind: string, input: any) => {
        calls.push({ kind, input }); active++; maxActive = Math.max(maxActive, active);
        if (kind === 'create') {
            const validate = (value: unknown): void => {
                if (Array.isArray(value)) { value.forEach(validate); return; }
                if (!value || typeof value !== 'object') return;
                const row = value as Record<string, unknown>;
                if ('element_id' in row) assert.match(String(row.element_id), /^[A-Za-z][A-Za-z0-9_]{0,19}$/, 'CardKit element IDs must satisfy the platform schema');
                Object.values(row).forEach(validate);
            };
            validate(JSON.parse(input.data.data));
        }
        await waitHeld(kind);
        await new Promise(resolve => setTimeout(resolve, 2)); active--;
        if (failure) throw failure;
        return { code: responseCode, data: { card_id: 'card-fixture' } };
    };
    const channel = { rawClient: { cardkit: { v1: { card: { create: (input: unknown) => request('create', input), update: (input: unknown) => request('structure', input) },
        cardElement: { content: (input: unknown) => request('text', input) } } } },
        send: async (...input: unknown[]) => { calls.push({ kind: 'send', input }); await waitHeld('send'); if (sendFailure) throw sendFailure; return { messageId: 'message-fixture' }; },
        updateCard: async (...input: unknown[]) => { calls.push({ kind: 'patch', input }); }
    } as unknown as Pick<LarkChannel, 'rawClient' | 'send' | 'updateCard'>;
    const logs: object[] = [];
    const make = () => createReplyCardStream(channel, store, (event, detail) => logs.push({ event, detail }));
    t.after(async () => { if (store.isOpen) store.close(); await rm(dir, { recursive: true, force: true }); });
    return { calls, logs, make, get store() { return store; }, fail(value: unknown) { failure = value; }, failSend(value: unknown) { sendFailure = value; }, code(value: number) { responseCode = value; },
        hold(kind: string) {
            let release!: () => void, entered!: () => void;
            const wait = new Promise<void>(resolve => { release = resolve; });
            const started = new Promise<void>(resolve => { entered = resolve; });
            held = { kind, entered, wait };
            return { started, release };
        },
        reopen() { store.close(); store = new Store(path); }, maxActive: () => maxActive };
}

test('one message streams both elements with persistent monotonic sequences and final structure', async t => {
    const f = await fixture(t), stream = f.make();
    const message = await stream.create(room, card(), 'trigger', 'thread');
    await stream.update(message, card('Answer', 'Reasoning'));
    await stream.update(message, card('Answer complete', 'Reasoning', false));
    assert.deepEqual(f.calls.map(c => c.kind), ['create', 'send', 'text', 'text', 'structure']);
    assert.deepEqual(f.calls.filter(c => ['text', 'structure'].includes(c.kind)).map(c => c.input.data.sequence), [1, 2, 3]);
    assert.deepEqual(f.calls.filter(c => ['text', 'structure'].includes(c.kind)).map(c => c.input.data.uuid), ['c_card-fixture_1', 'c_card-fixture_2', 'u_card-fixture_3']);
    assert.deepEqual(f.calls[1]!.input[1], { card: { type: 'card', data: { card_id: 'card-fixture' } } });
    assert.deepEqual(f.calls[1]!.input[2], { replyTo: 'trigger', replyInThread: true });
    assert.equal(JSON.parse(f.calls.at(-1)!.input.data.card.data).config.streaming_mode, false);
    f.reopen();
    await f.make().update(message, card('Recovered final', 'Reasoning', false));
    assert.equal(f.calls.at(-1)!.kind, 'structure');
    assert.equal(f.calls.at(-1)!.input.data.sequence, 4);
    assert.equal(f.calls.filter(c => c.kind === 'send').length, 1);
});

test('production reply cards satisfy CardKit element schema and stream nested reasoning on the same message', async t => {
    const f = await fixture(t), stream = f.make();
    const message = await stream.create(room, replyCard('处理中', 'First', 'Reasoning'));
    await stream.update(message, replyCard('处理中', 'First answer', 'Reasoning continues'));
    await stream.update(message, replyCard('处理中', 'First answer continues', 'Reasoning continues'));
    assert.deepEqual(f.calls.map(c => c.kind), ['create', 'send', 'structure', 'text']);
    assert.deepEqual(f.calls.filter(c => c.kind === 'text').map(c => c.input.path.element_id), ['reply_answer']);
    assert.equal(f.calls.filter(c => c.kind === 'send').length, 1);
});

test('failed API updates preserve snapshot and advance sequence without new messages or inline patch', async t => {
    const f = await fixture(t), stream = f.make(), message = await stream.create(room, card());
    const before = f.store.db.prepare('SELECT snapshot FROM feishu_cards').get()!.snapshot;
    f.code(1234);
    await assert.rejects(stream.update(message, card('retry')), /1234/);
    assert.equal(f.store.db.prepare('SELECT snapshot FROM feishu_cards').get()!.snapshot, before);
    f.code(0); await stream.update(message, card('retry'));
    assert.deepEqual(f.calls.filter(c => c.kind === 'text').map(c => c.input.data.sequence), [1, 2]);
    assert.equal(f.calls.filter(c => c.kind === 'send').length, 1);
    assert.equal(f.calls.some(c => c.kind === 'patch'), false);
});

test('concurrent updates serialize and identical snapshots do not issue requests', async t => {
    const f = await fixture(t), stream = f.make(), message = await stream.create(room, card());
    await Promise.all([stream.update(message, card('a')), stream.update(message, card('ab')), stream.update(message, card('abc'))]);
    await stream.update(message, card('abc'));
    assert.equal(f.maxActive(), 1);
    assert.deepEqual(f.calls.filter(c => c.kind === 'text').map(c => c.input.data.content), ['a', 'ab', 'abc']);
});

test('ordinary cards keep SDK send and update behavior', async t => {
    const f = await fixture(t), stream = f.make(), plain = { schema: '2.0', body: { elements: [] } };
    const message = await stream.create(room, plain);
    await stream.update(message, plain);
    assert.deepEqual(f.calls.map(c => c.kind), ['send', 'patch']);
});

test('closed transport permits only a disabling update to its own plain approval card', async t => {
    const f = await fixture(t), stream = f.make();
    const interactive = displayCard('需要补充信息', 'Question', [{ tag: 'button', value: { question: 0 } }]);
    const message = await stream.create(room, interactive);
    f.store.close();
    let sqlCalls = 0;
    t.mock.method(f.store.db, 'prepare', () => { sqlCalls++; throw new Error('closed SQLite touched'); });
    await assert.rejects(stream.update(message, interactive), /closed/);
    await assert.rejects(stream.update(message, displayCard('请求已结束', 'Not closed', [{ tag: 'button' }])), /closed/);
    await assert.rejects(stream.update('unknown-message', displayCard('请求已结束', 'Closed')), /closed/);
    await stream.update(message, displayCard('请求已结束', 'Closed'));
    assert.deepEqual(f.calls.map(c => c.kind), ['send', 'patch']);
    await assert.rejects(stream.update(message, displayCard('请求已结束', 'Repeated')), /closed/, 'successful cosmetic delivery removes its shutdown exception');
    assert.equal(sqlCalls, 0);
});

test('only explicit pre-send CardKit permission rejection allows inline fallback', async t => {
    const f = await fixture(t), stream = f.make();
    f.code(99991672); await stream.create(room, card());
    assert.deepEqual(f.calls.map(c => c.kind), ['create', 'send']);
    assert.ok(f.logs.length);
    f.calls.length = 0; f.code(1234);
    await assert.rejects(stream.create(room, card()), /1234/);
    assert.deepEqual(f.calls.map(c => c.kind), ['create']);
    f.calls.length = 0; f.code(0); f.fail(new Error('ambiguous network failure'));
    await assert.rejects(stream.create(room, card()), /ambiguous/);
    assert.deepEqual(f.calls.map(c => c.kind), ['create']);
});

test('ambiguous message send is never retried or replaced', async t => {
    const f = await fixture(t), stream = f.make();
    f.failSend({ response: { status: 403 } });
    await assert.rejects(stream.create(room, card()));
    assert.deepEqual(f.calls.map(c => c.kind), ['create', 'send']);
    assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM feishu_cards').get()!.n, 0);
});

for (const stage of ['create', 'send', 'text', 'structure'] as const) test(`late ${stage} response cannot touch closed SQLite or start another network request`, async t => {
    const f = await fixture(t), stream = f.make();
    let message = '';
    if (stage === 'text' || stage === 'structure') message = await stream.create(room, card());
    const held = f.hold(stage);
    const task = stage === 'text' ? stream.update(message, card('new answer', 'new thinking'))
        : stage === 'structure' ? stream.update(message, card('final answer', 'final thinking', false))
        : stream.create(room, card());
    // Observe rejections immediately while the mocked request is held.
    const outcome = task.then(value => ({ value }), error => ({ error }));
    await held.started;
    const count = f.calls.length;
    f.store.close();
    let closedSqlCalls = 0;
    t.mock.method(f.store.db, 'prepare', () => { closedSqlCalls++; throw new Error('closed SQLite was accessed'); });
    held.release();
    const result = await outcome;
    assert.equal(closedSqlCalls, 0);
    assert.equal(f.calls.length, count);
    if (stage === 'create' || stage === 'text') assert.ok('error' in result);
    if (stage === 'send') assert.deepEqual(result, { value: 'message-fixture' });
    if (stage === 'structure') assert.deepEqual(result, { value: undefined });
    await assert.rejects(stream.create(room, card()), /closed/);
    await assert.rejects(stream.update(message || 'unknown-message', card()), /closed/);
    assert.equal(closedSqlCalls, 0);
});
