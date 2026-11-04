import test from 'node:test';
import assert from 'node:assert/strict';
import { createLarkChannel, type LarkChannel } from '@larksuite/channel';
import { Approvals } from '../src/approvals.ts';
import { createReplyCardStream } from '../src/feishu-card-stream.ts';
import { route } from '../src/policy.ts';
import { fixture, message, until } from './helpers.ts';

async function questionFixture() {
    const f = await fixture(), room = route(f.config, message())!, session = f.store.session(room.key);
    const questions = [{ question: 'Pick colors', multiSelect: true, options: [{ label: 'Blue' }, { label: 'Green' }] }];
    const outcome = f.gateway.approvals.request(room, session.id, 'AskUserQuestion', {}, '', undefined, questions);
    await until(() => f.transport.sent.some(item => !!item.card));
    const sent = f.transport.sent.find(item => !!item.card)!;
    await until(() => !!f.store.db.prepare('SELECT 1 FROM outputs WHERE id=?').get(sent.id));
    const currentCard = () => f.transport.updates.filter(item => item.id === sent.id).at(-1)?.card ?? sent.card!;
    const values = () => (currentCard() as { body: { elements: { value?: Record<string, unknown> }[] } }).body.elements.filter(item => item.value).map(item => item.value!);
    const id = String(values()[0]!.request);
    return { ...f, room, session, outcome, sent, currentCard, values, id };
}

test('real pinned SDK accepts repeated question options and submit after empty submit without disabling dedup', async () => {
    const f = await questionFixture();
    const channel = createLarkChannel({ appId: 'cli_fixture', appSecret: 'fixture-not-real', safety: { chatQueue: { enabled: true, cardActions: 'separate' } } });
    // Test-only ingress replacement; production uses only public channel methods.
    const sdk = channel as unknown as { registerDispatcherHandlers(): void; dispatcher: { handles: Map<string, (event: unknown) => Promise<unknown>> }; safety: { dispose(): Promise<void> } };
    const toasts: string[] = [];
    channel.on('cardAction', async event => { const text = await f.gateway.approvals.click(event.chatId, event.operator.openId, event.action.value, event.messageId); toasts.push(text); return { toast: { type: 'info', content: text } }; });
    sdk.registerDispatcherHandlers();
    const dispatch = sdk.dispatcher.handles.get('card.action.trigger')!;
    let token = 0;
    const click = (value: object, operator = 'alice') => dispatch({ token: `unique-${++token}`, context: { open_message_id: f.sent.id, open_chat_id: f.room.chatId }, operator: { open_id: operator }, action: { tag: 'button', value } });
    try {
        const original = f.values()[2]!;
        await click(original); assert.equal(toasts.at(-1), '请回答所有问题');
        const afterEmpty = f.values()[0]!;
        assert.notEqual(original.revision, afterEmpty.revision);
        assert.equal(Object.keys(afterEmpty)[0], 'revision', 'SDK truncates serialized values to 128 characters');
        await click(afterEmpty); assert.match(toasts.at(-1)!, /Blue/);
        const count = toasts.length;
        await click(afterEmpty); assert.equal(toasts.length, count, 'duplicate delivery of the same button remains suppressed');
        await click(f.values()[1]!); assert.match(toasts.at(-1)!, /Blue, Green/);
        await click(f.values()[0]!); assert.match(toasts.at(-1)!, /→ Green$/);
        const revisions = f.transport.updates.map(item => JSON.stringify(item.card));
        assert.ok(revisions.every(text => !text.includes('✓') && !text.includes('已记录你的选择')), 'shared card must not expose private selections');
        await click(f.values()[2]!);
        assert.equal(toasts.at(-1), '已提交');
        assert.deepEqual((await f.outcome).answers, { 'Pick colors': 'Green' });
        assert.equal(f.transport.sent.length, 1, 'refresh the original message without new cards');
    } finally { await sdk.safety.dispose(); await f.close(); }
});

test('unauthorized clicks do not rotate question cards and refresh failures offer answer recovery', async () => {
    const f = await questionFixture();
    try {
        assert.equal(await f.gateway.approvals.click(f.room.chatId, 'stranger', f.values()[0], f.sent.id), '');
        assert.equal(await f.gateway.approvals.click('another-chat', 'alice', f.values()[0], f.sent.id), '');
        assert.equal(await f.gateway.approvals.click(f.room.chatId, 'alice', f.values()[0], 'another-message'), '');
        assert.equal(f.transport.updates.length, 0);
        f.transport.update = async () => { throw new Error('refresh failed'); };
        const toast = await f.gateway.approvals.click(f.room.chatId, 'alice', f.values()[0], f.sent.id);
        assert.match(toast, /已记录你的选择.*Blue/);
        assert.ok(toast.includes(`/answer ${f.id}`));
        assert.equal(f.gateway.approvals.answer(f.room, 'alice', f.id, 'Blue'), '回答已提交');
        assert.deepEqual((await f.outcome).answers, { 'Pick colors': 'Blue' });
        assert.equal(f.transport.sent.length, 1);
    } finally { await f.close(); }
});

test('pending question refresh cannot overwrite settlement even after SQLite closes', async () => {
    const f = await questionFixture();
    let release!: () => void, started!: () => void;
    const entered = new Promise<void>(resolve => { started = resolve; });
    const held = new Promise<void>(resolve => { release = resolve; });
    const applied: object[] = [];
    let first = true, closed = false;
    f.transport.update = async (_id, card) => { if (first) { first = false; started(); await held; } applied.push(card); };
    try {
        const clicking = f.gateway.approvals.click(f.room.chatId, 'alice', f.values()[0], f.sent.id);
        await entered;
        const queued = f.gateway.approvals.click(f.room.chatId, 'bob', f.values()[1], f.sent.id);
        f.gateway.approvals.cancelSession(f.session.id);
        assert.equal((await f.outcome).reason, 'cancelled');
        await f.gateway.close(); f.store.close(); closed = true;
        let sqlAfterClose = 0;
        const original = f.store.db.prepare;
        f.store.db.prepare = (() => { sqlAfterClose++; throw new Error('closed database touched'); }) as typeof original;
        release(); await Promise.all([clicking, queued]);
        await until(() => applied.length === 2);
        assert.equal(sqlAfterClose, 0);
        assert.match(JSON.stringify(applied.at(-1)), /请求已结束/);
        assert.equal(JSON.stringify(applied.at(-1)).includes('button'), false);
        assert.equal(f.transport.sent.length, 1);
    } finally {
        release();
        if (closed) { const { rm } = await import('node:fs/promises'); await rm(f.dir, { recursive: true, force: true }); }
        else await f.close();
    }
});

test('production CardKit helper permits only known plain approval disabling after a held refresh and closed Store', async () => {
    const f = await fixture(), room = route(f.config, message())!, session = f.store.session(room.key);
    let release!: () => void, entered!: () => void;
    const started = new Promise<void>(resolve => { entered = resolve; });
    const held = new Promise<void>(resolve => { release = resolve; });
    const applied: object[] = [];
    let updating = false, nativeCalls = 0, sends = 0, storeClosed = false;
    const channel = { rawClient: { cardkit: { v1: { card: { create: async () => { nativeCalls++; throw new Error('plain approvals must not create entities'); } } } } },
        send: async () => { sends++; return { messageId: 'plain-question-message' }; },
        updateCard: async (_message: string, card: object) => {
            if (!updating) { updating = true; entered(); await held; }
            applied.push(card);
        }
    } as unknown as Pick<LarkChannel, 'rawClient' | 'send' | 'updateCard'>;
    const stream = createReplyCardStream(channel, f.store, () => {});
    const approvals = new Approvals(f.store, { ...f.transport,
        card: stream.create, update: stream.update
    } as typeof f.transport, 300000);
    const outcome = approvals.request(room, session.id, 'AskUserQuestion', {}, '', undefined,
        [{ question: 'Color?', options: [{ label: 'Blue' }, { label: 'Green' }] }]);
    try {
        await until(() => !!f.store.db.prepare('SELECT 1 FROM outputs WHERE id=?').get('plain-question-message'));
        const id = String(f.store.db.prepare('SELECT id FROM requests').get()!.id);
        const clicking = approvals.click(room.chatId, 'alice', { request: id, question: 0, option: 0 }, 'plain-question-message');
        await started;
        approvals.cancelSession(session.id);
        assert.equal((await outcome).reason, 'cancelled');
        await f.gateway.close(); f.store.close(); storeClosed = true;
        let sqlCalls = 0;
        f.store.db.prepare = (() => { sqlCalls++; throw new Error('SQLite is closed'); }) as typeof f.store.db.prepare;
        release(); await clicking;
        await until(() => applied.length === 2);
        assert.equal(sqlCalls, 0);
        assert.equal(nativeCalls, 0); assert.equal(sends, 1);
        assert.match(JSON.stringify(applied.at(-1)), /请求已结束/);
        assert.equal(JSON.stringify(applied.at(-1)).includes('button'), false);
        await assert.rejects(stream.update('unknown-message', applied.at(-1)!), /closed/);
        assert.equal(sqlCalls, 0);
    } finally {
        release(); approvals.close();
        if (storeClosed) { const { rm } = await import('node:fs/promises'); await rm(f.dir, { recursive: true, force: true }); }
        else await f.close();
    }
});
