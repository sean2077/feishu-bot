import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { join } from 'node:path';
import { parseConfig } from '../src/config.ts';
import { Gateway, route } from '../src/gateway.ts';
import { Store } from '../src/store.ts';
import { Receipts } from '../src/receipts.ts';
import { NativeOutput } from '../src/native-output.ts';
import { normalizeHistoryItem, reactionHandler } from '../src/feishu.ts';
import { fixture, message, rawConfig, until } from './helpers.ts';
import type { Delivery, Message, WorkerEvent } from '../src/types.ts';

test('defaults: zero background and maintainers-only groups without members', async () => {
    const c = parseConfig({ ...rawConfig('state'), groups: [{ chatId: 'group1', projectId: 'p' }] });
    assert.equal(c.limits.backgroundMessages, 0);
    assert.equal(route(c, message()), undefined);
    assert.ok(route(c, message({ senderId: 'admin' })));
    assert.throws(() => parseConfig({ ...rawConfig('state'), groups: [{ chatId: 'group1', projectId: 'p', members: ['*'] }] }));
});
test('unauthorized and non-mentioned messages are silent with zero stored body, context and receipts', async () => {
    const f = await fixture();
    try {
        for (const m of [message({ senderId: 'outsider' }), message({ chatId: 'unknown' }), message({ mentioned: false }),
            message({ senderIsBot: true }), message({ chatType: 'p2p', chatId: 'private', senderId: 'outsider' }),
            message({ text: '/new', mentioned: false }), message({ text: '/compact', mentioned: false })]) await f.gateway.message(m);
        assert.equal(f.workers.length, 0); assert.equal(f.transport.sent.length, 0); assert.equal(f.transport.receipts.length, 0);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n, 0);
        await f.gateway.message(message({ chatType: 'p2p', chatId: 'dm', mentioned: false }));
        await until(() => f.workers[0]?.sent.length === 1);
    } finally { await f.close(); }
});
test('a reply without @ never triggers, even when replying to an own output', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!, session = f.store.session(room.key);
        f.store.recordOutput('own-card', room.key, session.id);
        await f.gateway.message(message({ mentioned: false, replyTo: 'own-card' }));
        assert.equal(f.workers.length, 0); assert.equal(f.store.countPending(room.key), 0);
    } finally { await f.close(); }
});
test('threads and different authors stay in one session; native @ input is not task-queued', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message({ threadId: 'thread-one' }));
        await until(() => f.workers[0]?.sent.length === 1);
        await f.gateway.message(message({ senderId: 'bob', threadId: 'thread-two' }));
        await until(() => f.workers[0]!.sent.length === 2);
        assert.equal(f.workers.length, 1);
        assert.equal(f.workers[0]!.sent[0]!.sessionId, f.workers[0]!.sent[1]!.sessionId);
    } finally { await f.close(); }
});
test('stop retains session ID; only new rotates it and cancels delayed work', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const native = randomUUID(), old = f.workers[0]!, session = old.sent[0]!.sessionId;
        old.emit({ type: 'ready', ccId: native });
        await f.gateway.message(message({ text: '/stop', senderId: 'admin' }));
        assert.equal(old.stopped, true);
        await f.gateway.message(message()); await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers[1]!.session.id, session); assert.equal(f.workers[1]!.session.ccId, native);
        await f.gateway.message(message({ text: '/new', senderId: 'admin' }));
        await f.gateway.message(message()); await until(() => f.workers[2]?.sent.length === 1);
        assert.notEqual(f.workers[2]!.session.id, session);
    } finally { await f.close(); }
});
test('compact is a durable turn-boundary barrier and normal input resumes after the native result', async () => {
    const f = await fixture();
    try {
        const first = message(), compact = message({ text: '/compact keep decisions', senderId: 'admin' }), after = message();
        await f.gateway.message(first); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        await f.gateway.message(compact); await f.gateway.message(after);
        assert.equal(w.sent.length, 1);
        w.emit({ type: 'result', text: 'first', success: true, uuids: [w.sent[0]!.uuid] });
        await until(() => w.sent.length === 2);
        assert.deepEqual(w.sent[1]!.command, { name: 'compact', instructions: 'keep decisions' });
        assert.equal(w.sent[1]!.message.id, compact.id);
        assert.equal(f.store.countPending(w.room.key), 2);
        w.emit({ type: 'result', text: 'Not enough messages to compact.', success: true, compact: 'not_needed', uuids: [w.sent[1]!.uuid] });
        await until(() => w.sent.length === 3);
        assert.equal(w.sent[2]!.message.id, after.id);
        assert.equal(new Set(w.sent.map(d => d.sessionId)).size, 1);
        assert.ok(f.transport.sent.some(s => s.text?.includes('轮次结束')));
    } finally { await f.close(); }
});
test('compaction queued during an approval does not block explicit approval answers', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'request', kind: 'permission', id: 'p', name: 'Bash', input: { command: 'test' } });
        await until(() => f.gateway.approvals.size(w.sent[0]!.sessionId) === 1);
        await f.gateway.message(message({ senderId: 'admin', text: '/compact' }));
        const request = String(f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id);
        const cardId = f.transport.sent.find(s => s.card)?.id;
        await f.gateway.approvals.click('group1', 'admin', { request, decision: 'allow' }, cardId);
        await until(() => w.responses.length === 1);
        assert.equal(w.sent.length, 1);
        w.emit({ type: 'result', text: 'done', success: true, uuids: [w.sent[0]!.uuid] });
        await until(() => w.sent.length === 2);
    } finally { await f.close(); }
});
test('history is explicit, scoped, author-filtered, bounded and cursor-bound', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        assert.equal(f.transport.historyCalls.length, 0);
        const w = f.workers[0]!, d = w.sent[0]!;
        const allowed = message({ mentioned: false, text: 'relevant' });
        f.transport.pages.push({ messages: [allowed, message({ senderId: 'outsider', text: 'private' }), message({ chatId: 'group2', text: 'other group' }), message({ senderIsBot: true, senderIsSelf: true, senderId: 'this-bot', text: 'prior answer' })], nextPageToken: 'opaque-token' });
        const result = await f.gateway.history.read(w.room, d.sessionId, { limit: 20 });
        assert.equal((result.messages as Message[]).length, 2);
        assert.ok(!JSON.stringify(result).includes('private')); assert.ok(!JSON.stringify(result).includes('other group'));
        assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM inbox WHERE status='background'").get()!.n, 0);
        const other = route(f.config, message({ chatId: 'group2' }))!;
        await assert.rejects(f.gateway.history.read(other, d.sessionId, { cursor: result.nextCursor }), /cursor/);
        await assert.rejects(f.gateway.history.read(w.room, 'different-session', { cursor: result.nextCursor }), /cursor/);
        await assert.rejects(f.gateway.history.read(w.room, d.sessionId, { chatId: 'group2' }), /Unsupported/);
        await assert.rejects(f.gateway.history.read(w.room, d.sessionId, { limit: 500 }), /limit/);
        await f.gateway.history.read(w.room, d.sessionId, { cursor: result.nextCursor });
        assert.equal(f.transport.historyCalls.at(-1)!.query.pageToken, 'opaque-token');
    } finally { await f.close(); }
});
test('history permission errors are not silently reported as empty history', async () => {
    const f = await fixture();
    try {
        f.transport.history = async () => { throw new Error('missing message-read permissions'); };
        const room = route(f.config, message())!;
        await assert.rejects(f.gateway.history.read(room, 's', {}), /permissions/);
        await assert.rejects(f.gateway.history.read(room, 's', { threadId: 'unseen' }), /Thread/);
    } finally { await f.close(); }
});
test('quote expansion and remembered attachment metadata cannot bypass author authorization', async () => {
    const f = await fixture();
    try {
        const quote = message({ senderId: 'outsider', text: 'forbidden', resources: [{ key: 'secret-file', kind: 'file', name: 'secret.txt' }] });
        f.transport.quoted.set(quote.id, quote);
        await f.gateway.message(message({ replyTo: quote.id })); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        assert.equal(w.sent[0]!.quote, undefined);
        f.store.rememberContext(w.room.key, [quote]);
        w.emit({ type: 'request', id: 'attachment', kind: 'attachment', name: 'fetch_attachment', input: { messageId: quote.id, key: 'secret-file' } });
        await until(() => w.responses.length === 1);
        assert.equal(w.responses[0]!.value.ok, false);
    } finally { await f.close(); }
});
test('recovery strips previously queued background and rejects a revoked sender', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!, session = f.store.session(room.key);
        const m = message(), bad = message({ senderId: 'revoked' });
        for (const msg of [m, bad]) {
            const d: Delivery = { uuid: randomUUID(), roomKey: room.key, sessionId: session.id, message: msg,
                background: [message({ mentioned: false, text: 'old hidden context' })], attachments: [] };
            f.store.insert(room.key, msg, 'queued'); f.store.queue(d);
        }
        await f.gateway.recover(); await until(() => f.workers[0]?.sent.length === 1);
        assert.deepEqual(f.workers[0]!.sent[0]!.background, []);
        assert.equal(f.store.db.prepare('SELECT message FROM inbox WHERE id=?').get(bad.id)!.message, '{}');
    } finally { await f.close(); }
});
test('a restarted gateway resumes the saved native ID without replaying uncertain inputs', async () => {
    const f = await fixture();
    let restarted: Gateway | undefined;
    try {
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, id = randomUUID(), old = w.sent[0]!;
        w.emit({ type: 'ready', ccId: id });
        await f.gateway.close(); f.store.close();
        const db = new Store(join(f.dir, 'state.sqlite'));
        restarted = new Gateway(f.config, db, f.transport, f.factory);
        await restarted.recover();
        assert.equal(f.workers.length, 1);
        await restarted.message(message()); await until(() => f.workers[1]?.sent.length === 1);
        assert.equal(f.workers[1]!.session.ccId, id); assert.equal(f.workers[1]!.session.id, old.sessionId);
        assert.equal(db.db.prepare('SELECT status FROM inbox WHERE uuid=?').get(old.uuid)!.status, 'uncertain');
        await restarted.close(); restarted = undefined; db.close();
    } finally {
        await restarted?.close();
        // The first Store is already closed.
        const { rm } = await import('node:fs/promises'); await rm(f.dir, { recursive: true, force: true });
    }
});
test('reaction completion wins a late processing response, and failed delivery never sets Done', async () => {
    const f = await fixture();
    try {
        let release!: () => void;
        const states: string[] = [];
        f.transport.receipt = async (_id, state = 'processing') => { if (state === 'processing') await new Promise<void>(r => { release = r; }); states.push(state); };
        await f.gateway.message(message()); await until(() => !!release && f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'result', text: 'answer', success: true, uuids: [w.sent[0]!.uuid] });
        await until(() => f.transport.sent.some(s => s.card));
        release(); await f.gateway.receipts.drain();
        assert.equal(states.at(-1), 'done');
        f.transport.receipt = async (_id, state) => { states.push(state!); };
        f.transport.card = async () => { throw new Error('ambiguous create'); };
        await f.gateway.message(message()); await until(() => w.sent.length === 2);
        w.emit({ type: 'result', text: 'lost', success: true, uuids: [w.sent[1]!.uuid] });
        await until(() => !!f.store.db.prepare("SELECT id FROM outbox WHERE state='uncertain'").get());
        await f.gateway.receipts.drain(); assert.equal(states.at(-1), 'clear');
    } finally { await f.close(); }
});
test('one card preserves reasoning and final answer, suppresses tools, and quotes the first input', async () => {
    const f = await fixture();
    try {
        const first = message(), second = message({ senderId: 'bob' });
        await f.gateway.message(first); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, turnId = w.sent[0]!.uuid;
        w.emit({ type: 'thinking', text: 'Reasoning summary', partial: true, blockId: 'm:0', turnId });
        w.emit({ type: 'thinking', text: 'Reasoning summary', partial: false, blockId: 'm:0', turnId });
        w.emit({ type: 'activity', tool: 'NEVER_DISPLAY_THIS_TOOL' });
        await until(() => f.transport.sent.some(s => s.card));
        const card = f.transport.sent.find(s => s.card)!;
        await f.gateway.message(second); await until(() => w.sent.length === 2);
        w.emit({ type: 'text', text: 'Final answer', partial: true, blockId: 'm:1', turnId });
        w.emit({ type: 'text', text: 'Final answer', partial: false, blockId: 'm:1', turnId });
        w.emit({ type: 'result', text: 'Final answer', success: true, uuids: w.sent.map(d => d.uuid) });
        await until(() => f.transport.updates.some(u => JSON.stringify(u.card).includes('Final answer')));
        assert.equal(f.transport.sent.filter(s => s.card).length, 1); assert.equal(card.replyTo, first.id);
        const final = JSON.stringify(f.transport.updates.at(-1)!.card);
        assert.equal(final.split('Reasoning summary').length, 2); assert.equal(final.split('Final answer').length, 2);
        assert.ok(final.includes('"expanded":false')); assert.ok(!final.includes('NEVER_DISPLAY_THIS_TOOL'));
        await f.gateway.receipts.drain();
        assert.ok(f.transport.receiptStates.some(r => r.id === second.id && r.state === 'done'));
    } finally { await f.close(); }
});
test('native output block IDs deduplicate stream snapshots and never expose signatures or tool traces', () => {
    const events: WorkerEvent[] = [], output = new NativeOutput(e => events.push(e));
    output.handle({ type: 'stream_event', user_message_uuid: 'input', event: { type: 'message_start', message: { id: 'message' } } });
    output.handle({ type: 'stream_event', user_message_uuid: 'input', event: { type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: 'public reasoning' } } });
    output.handle({ type: 'stream_event', event: { type: 'content_block_delta', index: 0, delta: { type: 'signature_delta', signature: 'secret-signature' } } });
    output.handle({ type: 'assistant', user_message_uuid: 'input', message: { id: 'message', content: [{ type: 'thinking', thinking: 'public reasoning', signature: 'secret-signature' }, { type: 'tool_use', name: 'hidden', input: { secret: 'x' } }] } });
    assert.equal(events.length, 2);
    assert.equal((events[0] as any).blockId, (events[1] as any).blockId);
    assert.ok(!JSON.stringify(events).includes('secret-signature')); assert.ok(!JSON.stringify(events).includes('hidden'));
});
test('raw history normalization keeps authoritative chat, author and quote fields', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        const raw = { message_id: 'om_history', chat_id: 'group1', parent_id: 'om_parent', thread_id: 'omt_thread',
            sender: { id: 'alice', id_type: 'open_id', sender_type: 'user' }, msg_type: 'text', create_time: '1700000000000', body: { content: '{"text":"history"}' } };
        const m = await normalizeHistoryItem(raw, room, 'bot', 'app');
        assert.equal(m!.chatId, room.chatId); assert.equal(m!.replyTo, 'om_parent'); assert.equal(m!.threadId, 'omt_thread');
        assert.equal(await normalizeHistoryItem({ ...raw, chat_id: 'group2' }, room), undefined);
        assert.equal(await normalizeHistoryItem({ ...raw, sender: { ...raw.sender, id: 'outsider' } }, room), undefined);
    } finally { await f.close(); }
});
test('reaction cleanup never deletes another app reaction', async () => {
    const removed: string[] = [], added: string[] = [];
    const fake = { botIdentity: { openId: 'bot' }, removeReaction: async (_id: string, rid: string) => { removed.push(rid); },
        addReaction: async (_id: string, emoji: string) => { added.push(emoji); return 'created'; },
        rawClient: { im: { v1: { messageReaction: { list: async () => ({ code: 0, data: { items: [
            { reaction_id: 'other', operator: { operator_type: 'app', operator_id: 'other-app' } },
            { reaction_id: 'mine', operator: { operator_type: 'app', operator_id: 'app' } }
        ] } }) } } } } };
    await reactionHandler(fake as any, 'app')('m', 'done');
    assert.deepEqual(removed, ['mine']); assert.deepEqual(added, ['Done']);
});

test('pending final content is persisted before slow preview I/O and prevents idle eviction', async () => {
    const f = await fixture();
    let release: (() => void) | undefined;
    try {
        f.config.runtime.idleMs = 1;
        const create = f.transport.card.bind(f.transport);
        f.transport.card = async (...args) => { await new Promise<void>(r => { release = r; }); return create(...args); };
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, turnId = w.sent[0]!.uuid;
        w.emit({ type: 'text', text: 'preview', partial: true, blockId: 'slow:0', turnId });
        await until(() => !!release);
        w.emit({ type: 'activity', background: 0, pinned: false, state: 'idle' });
        w.emit({ type: 'result', text: 'persist this final', success: true, uuids: [turnId] });
        await until(() => !!f.store.db.prepare("SELECT id FROM outbox WHERE state='preparing'").get());
        assert.equal(f.store.db.prepare("SELECT text FROM outbox WHERE state='preparing'").get()!.text, 'persist this final');
        await f.gateway.sweep(Date.now() + 100000); assert.equal(w.stopped, false);
        release!(); release = undefined;
        await until(() => !!f.store.db.prepare("SELECT id FROM outbox WHERE state='done'").get());
        assert.equal(f.transport.sent.filter(s => s.card).length, 1);
    } finally { release?.(); await f.close(); }
});

test('stop during final delivery cannot turn cancellation into a successful Done receipt', async () => {
    const f = await fixture(); let release: (() => void) | undefined;
    try {
        const create = f.transport.card.bind(f.transport);
        f.transport.card = async (...args) => { await new Promise<void>(r => { release = r; }); return create(...args); };
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, d = w.sent[0]!;
        w.emit({ type: 'result', text: 'late final', success: true, uuids: [d.uuid] });
        await until(() => !!release);
        await f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        release!(); release = undefined;
        await until(() => f.store.db.prepare('SELECT state FROM outbox').get()!.state === 'cancelled');
        await f.gateway.receipts.drain();
        assert.ok(!f.transport.receiptStates.some(r => r.id === d.message.id && r.state === 'done'));
        assert.equal(f.store.session(w.room.key).id, d.sessionId);
    } finally { release?.(); await f.close(); }
});

test('revoked senders cannot receive pending final output or a completion reaction', async () => {
    const f = await fixture();
    try {
        const m = message(), room = route(f.config, m)!, session = f.store.session(room.key);
        f.gateway.receipts.accept(room, session.id, m); await f.gateway.receipts.drain();
        const id = f.gateway.outbox.save(room, session.id, 'do not disclose', true, undefined, m.id, undefined, undefined, [m]);
        f.config.groups[0]!.members = [];
        assert.equal(await f.gateway.outbox.deliver(id, room), false);
        await f.gateway.receipts.drain();
        assert.equal(f.transport.sent.length, 0);
        assert.equal(f.store.db.prepare('SELECT state FROM outbox WHERE id=?').get(id)!.state, 'cancelled');
        assert.equal(f.transport.receiptStates.at(-1)!.state, 'clear');
    } finally { await f.close(); }
});

test('history rechecks replaced author policy after the API response', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!;
        f.transport.history = async () => {
            f.config.groups[0]!.members = [];
            return { messages: [message({ text: 'revoked while waiting' })] };
        };
        const result = await f.gateway.history.read(room, 'session', {});
        assert.deepEqual(result.messages, []);
    } finally { await f.close(); }
});

test('unstamped native deltas and individual completed blocks retain turn and block identity', () => {
    const events: WorkerEvent[] = [], output = new NativeOutput(e => events.push(e));
    output.handle({ type: 'stream_event', user_message_uuid: 'origin', event: { type: 'message_start', message: { id: 'm' } } });
    output.handle({ type: 'stream_event', event: { type: 'content_block_start', index: 0, content_block: { type: 'thinking', thinking: '' } } });
    output.handle({ type: 'stream_event', event: { type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: 'R' } } });
    output.handle({ type: 'assistant', message: { id: 'm', content: [{ type: 'thinking', thinking: 'R' }] } });
    output.handle({ type: 'stream_event', event: { type: 'content_block_start', index: 1, content_block: { type: 'text', text: '' } } });
    output.handle({ type: 'stream_event', event: { type: 'content_block_delta', index: 1, delta: { type: 'text_delta', text: 'A' } } });
    output.handle({ type: 'assistant', message: { id: 'm', content: [{ type: 'text', text: 'A' }] } });
    const text = events.filter(e => e.type === 'text');
    assert.ok(text.every(e => e.turnId === 'origin' && e.blockId === 'm:1'));
    assert.ok(events.filter(e => e.type === 'thinking').every(e => e.blockId === 'm:0'));
});
