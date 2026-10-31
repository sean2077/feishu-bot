import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { parseConfig } from '../dist/config.js';
import { prepareWorkspace } from '../dist/workspace.js';
import { runtimeFactory } from '../dist/runtime.js';
import { Gateway, route } from '../dist/gateway.js';
import { Store } from '../dist/store.js';

async function fixture() {
    const root = await mkdtemp(join(tmpdir(), 'feishu-contract-'));
    const previous = process.env.CONTRACT_FIXTURE_TOKEN;
    process.env.CONTRACT_FIXTURE_TOKEN = 'local-fixture-only';
    const requests = [], events = [], logs = [], workers = [];
    let phase = 'normal', historyRequested = false, requestId = 0, streamDelay = 0;
    const server = http.createServer(async (req, res) => {
        const chunks = []; for await (const part of req) chunks.push(part);
        const body = JSON.parse(Buffer.concat(chunks).toString());
        if (req.url.includes('count_tokens')) { res.end('{"input_tokens":6000}'); return; }
        requests.push(body);
        const content = phase === 'compact'
            ? [{ type: 'text', text: 'CONTRACT_MEMORY_MARKER: previous requests established the shared context; preserve it.' }]
            : !historyRequested
                ? (historyRequested = true, [{ type: 'tool_use', id: 'toolu_history', name: 'mcp__gateway__read_chat_history', input: { limit: 3 } }])
                : [{ type: 'thinking', thinking: 'Visible provider summary for the fixture.', signature: 'fixture-signature-must-not-be-displayed' }, { type: 'text', text: 'CONTRACT_MEMORY_MARKER: fixture response.' }];
        const stop = content.at(-1).type === 'tool_use' ? 'tool_use' : 'end_turn';
        const msg = { id: 'msg_fixture_' + (++requestId), type: 'message', role: 'assistant', model: 'fixture-model', content,
            stop_reason: stop, stop_sequence: null, usage: { input_tokens: 6000, output_tokens: 200 } };
        if (!body.stream) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify(msg)); return; }
        res.writeHead(200, { 'content-type': 'text/event-stream' });
        const emit = (type, value) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...value })}\n\n`);
        emit('message_start', { message: { ...msg, content: [], stop_reason: null, usage: { input_tokens: 6000, output_tokens: 0 } } });
        for (const [index, block] of content.entries()) {
            const start = block.type === 'thinking' ? { type: 'thinking', thinking: '', signature: '' }
                : block.type === 'text' ? { type: 'text', text: '' } : { ...block, input: {} };
            emit('content_block_start', { index, content_block: start });
            emit('content_block_delta', { index, delta: block.type === 'thinking' ? { type: 'thinking_delta', thinking: block.thinking }
                : block.type === 'text' ? { type: 'text_delta', text: block.text } : { type: 'input_json_delta', partial_json: JSON.stringify(block.input) } });
            if (block.type === 'thinking') emit('content_block_delta', { index, delta: { type: 'signature_delta', signature: block.signature } });
            emit('content_block_stop', { index });
            if (block.type === 'thinking' && streamDelay) await new Promise(resolve => setTimeout(resolve, streamDelay));
        }
        emit('message_delta', { delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: 200 } });
        emit('message_stop', {}); res.end();
    });
    await new Promise(r => server.listen(0, '127.0.0.1', r));
    const config = parseConfig({ tenantId: 'fixture', appId: 'cli_fixture', dataDir: root,
        model: { baseUrl: `http://127.0.0.1:${server.address().port}`, model: 'fixture-model', tokenEnv: 'CONTRACT_FIXTURE_TOKEN', allowHttp: true },
        projects: [{ id: 'p', maintainers: ['admin'] }], groups: [], dms: [] });
    const room = { key: 'room', chatId: 'chat', projectId: 'p', maintainers: ['admin'] };
    const session = { id: randomUUID(), roomKey: room.key, state: 'open', createdAt: Date.now() };
    const workspace = await prepareWorkspace(config, room, session, config.projects[0]);
    const start = async () => {
        let worker;
        worker = await runtimeFactory(config, ['local-fixture-only'], (event, detail) => logs.push({ event, detail }))(room, session, event => {
            events.push(event);
            if (event.type === 'ready') session.ccId = event.ccId;
            if (event.type === 'request') {
                worker.respond(event.id, event.kind === 'history' ? { messages: [{ id: 'fixture-history', text: 'HISTORY_TOOL_RESULT', senderId: 'admin' }], hasMore: false }
                    : { approved: false, error: 'Unexpected request in fixture' });
            }
        }, workspace);
        workers.push(worker); return worker;
    };
    const delivery = (text, compact = false) => ({ uuid: randomUUID(), roomKey: room.key, sessionId: session.id,
        message: { id: randomUUID(), chatId: room.chatId, chatType: 'group', senderId: 'admin', text, time: Date.now(), mentioned: true, resources: [] },
        background: [], attachments: [], ...(compact ? { command: { name: 'compact', instructions: 'Preserve CONTRACT_MEMORY_MARKER' } } : {}) });
    const waitFor = async (predicate, startAt = 0, timeout = 30000) => {
        const start = Date.now();
        while (Date.now() - start < timeout) {
            const matches = events.slice(startAt).find(predicate); if (matches) return matches;
            const error = events.slice(startAt).find(e => e.type === 'error');
            if (error) throw new Error(error.message + '\n' + JSON.stringify(logs));
            await new Promise(r => setTimeout(r, 10));
        }
        throw new Error('Native contract timeout\n' + JSON.stringify({ events, logs }));
    };
    const send = async (worker, d) => { const index = events.length; await worker.send(d); return waitFor(e => e.type === 'result' && e.uuids?.includes(d.uuid), index); };
    return { config, room, session, events, requests, logs, start, delivery, send, waitFor,
        setPhase(value) { phase = value; },
        setStreamingDelay(value) { streamDelay = value; },
        close: async () => { for (const w of workers) await w.stop(); server.closeAllConnections(); await new Promise(r => server.close(r));
            await rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
            if (previous === undefined) delete process.env.CONTRACT_FIXTURE_TOKEN; else process.env.CONTRACT_FIXTURE_TOKEN = previous;
        } };
}

test('real CC resumes after process restart, invokes scoped history, streams thinking and runs native compact', { timeout: 90000 }, async () => {
    const f = await fixture();
    try {
        const w = await f.start();
        const emptyCompact = await f.send(w, f.delivery('/compact', true));
        assert.equal(emptyCompact.compact, 'not_needed');
        assert.equal(f.requests.length, 0, 'empty compact is a native command, not a model prompt');
        const firstInput = f.delivery('CONTRACT_MEMORY_MARKER: first human request.');
        const first = await f.send(w, firstInput);
        assert.equal(first.success, true);
        assert.ok(f.events.some(e => e.type === 'request' && e.kind === 'history'));
        assert.ok(JSON.stringify(f.requests.at(-1)).includes('HISTORY_TOOL_RESULT'));
        const thinking = f.events.filter(e => e.type === 'thinking');
        assert.ok(thinking.some(e => e.text.includes('Visible provider summary')));
        assert.ok(!JSON.stringify(thinking).includes('fixture-signature'));
        const display = f.events.filter(e => e.type === 'text' || e.type === 'thinking');
        assert.ok(display.filter(e => e.text.includes('fixture response') || e.text.includes('Visible provider summary')).every(e => e.turnId === firstInput.uuid), 'unstamped deltas retain the first frame attribution');
        const blocks = new Map();
        for (const e of display) blocks.set(e.blockId, { kind: e.type, text: e.partial ? (blocks.get(e.blockId)?.text ?? '') + e.text : e.text });
        assert.equal([...blocks.values()].filter(b => b.kind === 'thinking' && b.text.includes('Visible provider summary')).length, 1);
        assert.equal([...blocks.values()].filter(b => b.kind === 'text' && b.text.includes('fixture response')).length, 1);
        const id = f.session.ccId; assert.ok(id);
        await w.stop();
        const count = f.requests.length, restarted = await f.start();
        const second = await f.send(restarted, f.delivery('Continue the earlier conversation.'));
        assert.equal(second.success, true); assert.equal(f.session.ccId, id);
        assert.ok(JSON.stringify(f.requests[count].messages).includes('first human request'), 'native transcript is loaded into the new process');
        f.setPhase('compact');
        const compact = await f.send(restarted, f.delivery('/compact', true));
        assert.equal(compact.compact, 'compacted', JSON.stringify(f.events));
        assert.ok(f.events.some(e => e.type === 'compaction' && e.phase === 'complete' && e.trigger === 'manual'));
        assert.equal(f.session.ccId, id);
        f.setPhase('normal');
        const result = await f.send(restarted, f.delivery('Continue after manual compact.'));
        assert.equal(result.success, true); assert.equal(f.session.ccId, id);
        assert.ok(JSON.stringify(f.requests.at(-1).messages).includes('CONTRACT_MEMORY_MARKER'));
    } finally { await f.close(); }
});

test('missing native resume ID fails rather than creating an unannounced new conversation', { timeout: 45000 }, async () => {
    const f = await fixture();
    try {
        const id = randomUUID(); f.session.ccId = id;
        const worker = await f.start(); await worker.send(f.delivery('Do not replace the missing transcript.'));
        const event = await f.waitFor(e => e.type === 'error' || e.type === 'exit');
        assert.ok(['error', 'exit'].includes(event.type));
        assert.equal(f.events.some(e => e.type === 'ready'), false);
        assert.equal(f.session.ccId, id);
        assert.equal(f.requests.length, 0);
    } finally { await f.close(); }
});


test('full native gateway keeps one attributed card and restores its persisted conversation after restart', { timeout: 60000 }, async () => {
    const f = await fixture();
    f.config.groups = [{ chatId: 'chat', projectId: 'p', members: ['alice'] }];
    f.config.limits.updateMs = 250; f.setStreamingDelay(400);
    const cards = [], updates = [], reactions = [];
    const transport = {
        async send(room, text, replyTo) { const id = randomUUID(); cards.push({ id, room, text, replyTo }); return id; },
        async card(room, card, replyTo) { const id = randomUUID(); cards.push({ id, room, card, replyTo }); return id; },
        async update(id, card) { updates.push({ id, card }); },
        async receipt(id, state) { reactions.push({ id, state }); },
        async history(room) { return { messages: [{ id: 'history', chatId: room.chatId, chatType: 'group', senderId: 'alice', text: 'HISTORY_TOOL_RESULT', resources: [], time: Date.now(), mentioned: false }] }; },
        async fetch() { return undefined; }, async download() { return Buffer.from('fixture'); }, async file() { return randomUUID(); }
    };
    const wait = async check => { const started = Date.now(); while (!check()) { if (Date.now() - started > 20000) throw new Error('gateway result timeout: ' + JSON.stringify({ cards, updates, reactions, logs: f.logs })); await new Promise(r => setTimeout(r, 10)); } };
    const message = text => ({ id: randomUUID(), chatId: 'chat', chatType: 'group', senderId: 'alice', text, resources: [], time: Date.now(), mentioned: true });
    let store = new Store(join(f.config.dataDir, 'gateway.sqlite'));
    const launch = () => new Gateway(f.config, store, transport, runtimeFactory(f.config, [], (event, detail) => f.logs.push({ event, detail })));
    let gateway = launch();
    try {
        await gateway.recover();
        const first = message('CONTRACT_MEMORY_MARKER first gateway input');
        await gateway.message(first);
        await wait(() => reactions.some(r => r.id === first.id && r.state === 'done'));
        const mainCards = cards.filter(c => c.card); assert.equal(mainCards.length, 1);
        assert.equal(mainCards[0].replyTo, first.id);
        const final = updates.filter(u => u.id === mainCards[0].id).at(-1)?.card ?? mainCards[0].card;
        assert.equal(final.body.elements[0].tag, 'collapsible_panel'); assert.equal(final.body.elements[0].expanded, false);
        assert.ok(JSON.stringify(final).includes('Visible provider summary'));
        assert.ok(JSON.stringify(final).includes('CONTRACT_MEMORY_MARKER'));
        assert.ok(!JSON.stringify(final).includes('signature-must-not-be-displayed'));
        assert.ok(!JSON.stringify(final).includes('toolu_history'));
        const room = route(f.config, first), session = store.session(room.key);
        assert.ok(session.ccId);
        await gateway.close(); store.close();
        const before = f.requests.length;
        store = new Store(join(f.config.dataDir, 'gateway.sqlite')); gateway = launch();
        await gateway.recover();
        const second = message('Continue after the service restarted.'); await gateway.message(second);
        await wait(() => reactions.some(r => r.id === second.id && r.state === 'done'));
        assert.equal(store.session(room.key).id, session.id); assert.equal(store.session(room.key).ccId, session.ccId);
        assert.ok(JSON.stringify(f.requests[before].messages).includes('first gateway input'));
        assert.equal(cards.filter(c => c.card).length, 2);
        assert.equal(cards.filter(c => c.card)[1].replyTo, second.id);
    } finally { await gateway.close(); store.close(); await f.close(); }
});
