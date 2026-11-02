import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { parseConfig } from '../dist/config.js';
import { Store } from '../dist/store.js';
import { Gateway } from '../dist/gateway.js';
import { runtimeFactory } from '../dist/runtime.js';
import { feishuTransport } from '../dist/feishu.js';
import { bindFeishuInbound } from '../dist/feishu-events.js';

export const fields = ['input_tokens', 'output_tokens', 'cache_read_input_tokens', 'cache_creation_input_tokens'];
export const sumUsage = records => Object.fromEntries(fields.map(field => [field, records.reduce((n, r) => n + r.usage[field], 0)]));
export function deferred() { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; }
export async function until(check, label, timeout = 30000) {
    const end = Date.now() + timeout;
    while (!check()) {
        if (Date.now() > end) throw new Error(`Timed out: ${label}`);
        await new Promise(r => setTimeout(r, 10));
    }
}
export function envelopes(body) {
    const values = [];
    for (const message of body.messages ?? []) {
        if (message.role !== 'user') continue;
        for (const block of typeof message.content === 'string' ? [{ type: 'text', text: message.content }] : message.content ?? []) {
            if (block.type !== 'text') continue;
            try { const value = JSON.parse(block.text); if (value.trigger || value.imageBatch) values.push(value); } catch {}
        }
    }
    return values;
}
async function respond(res, body, plan, usage) {
    const content = plan.content ?? [{ type: 'text', text: plan.text ?? 'fixture answer' }];
    const stop = content.some(block => block.type === 'tool_use') ? 'tool_use' : 'end_turn';
    const message = { id: `msg_${randomUUID()}`, type: 'message', role: 'assistant', model: plan.model ?? 'fixture-reported-model', content, stop_reason: stop, stop_sequence: null, usage };
    if (!body.stream) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify(message)); return; }
    res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
    const send = event => res.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
    send({ type: 'message_start', message: { ...message, content: [], stop_reason: null, usage: { ...usage, output_tokens: 0 } } });
    for (const [index, block] of content.entries()) {
        const initial = block.type === 'tool_use' ? { ...block, input: {} }
            : block.type === 'thinking' ? { type: 'thinking', thinking: '', signature: '' } : { type: 'text', text: '' };
        send({ type: 'content_block_start', index, content_block: initial });
        if (block.type === 'tool_use') send({ type: 'content_block_delta', index, delta: { type: 'input_json_delta', partial_json: JSON.stringify(block.input) } });
        else if (block.type === 'thinking') {
            send({ type: 'content_block_delta', index, delta: { type: 'thinking_delta', thinking: block.thinking } });
            send({ type: 'content_block_delta', index, delta: { type: 'signature_delta', signature: 'fixture-signature-not-display-content' } });
        } else send({ type: 'content_block_delta', index, delta: { type: 'text_delta', text: block.text } });
        send({ type: 'content_block_stop', index });
        if (plan.afterBlock) await plan.afterBlock(index);
    }
    send({ type: 'message_delta', delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: usage.output_tokens } });
    send({ type: 'message_stop' }); res.end();
}

/** Real Gateway/SQLite/native CC and Channel SDK dispatch. Only network ingress, Feishu transport and model intelligence are substitutes. */
export async function migrationFixture(planResponse, { actions = false } = {}) {
    const root = await mkdtemp(join(tmpdir(), 'feishu-migration-'));
    const marker = join(root, 'operation-count.txt');
    const f = { root, marker, mode: 'plain', requests: [], errors: [], events: [], workers: [], sent: [], updates: [], receipts: [], files: [], downloads: [], logs: [], quotes: new Map(), resources: new Map(), handled: new Set(), rejected: new Map(), closed: false, onFile: undefined, onEvent: undefined };
    const modelServer = createServer(async (req, res) => {
        try {
            const chunks = []; let size = 0;
            for await (const chunk of req) { size += chunk.length; if (size > 8 * 1024 * 1024) throw new Error('fixture request bound'); chunks.push(chunk); }
            const body = JSON.parse(Buffer.concat(chunks).toString() || '{}');
            if (req.url?.includes('/count_tokens')) { res.writeHead(200, { 'content-type': 'application/json' }); res.end('{"input_tokens":6000}'); return; }
            if (!req.url?.startsWith('/v1/messages')) { res.writeHead(404); res.end(); return; }
            const number = f.requests.length + 1;
            const usage = { input_tokens: 100 + number * 7, output_tokens: 13 + number, cache_read_input_tokens: number * 2, cache_creation_input_tokens: number * 3 };
            const record = { number, mode: f.mode, body, usage };
            f.requests.push(record);
            let plan;
            try { plan = await planResponse(f, record); }
            catch (error) { f.errors.push(String(error)); plan = { text: 'FIXTURE_PLAN_ERROR' }; }
            if (!res.destroyed) await respond(res, body, plan ?? {}, usage);
        } catch (error) {
            f.errors.push(String(error)); if (!res.destroyed) { res.writeHead(500); res.end('{"error":"fixture failure"}'); }
        }
    });
    await new Promise(resolve => modelServer.listen(0, '127.0.0.1', resolve));
    const modelEnv = 'MIGRATION_FIXTURE_MODEL_TOKEN', appEnv = 'MIGRATION_FIXTURE_APP_SECRET';
    const previous = [process.env[modelEnv], process.env[appEnv]];
    process.env[modelEnv] = 'fixture-model-token'; process.env[appEnv] = 'fixture-app-token';
    f.config = parseConfig({ tenantId: 'fixture', appId: 'cli_fixture', appSecretEnv: appEnv, dataDir: root,
        runtime: { maxWorkers: 2, idleMs: 0 }, model: { baseUrl: `http://127.0.0.1:${modelServer.address().port}`, allowHttp: true, tokenEnv: modelEnv, model: 'fixture-configured-alias' },
        projects: [{ id: 'fixture', maintainers: ['admin'], permissionMode: 'default', actions: actions ? [{
            id: 'record_once', description: 'Append one local fixture marker after approval', executable: process.execPath,
            args: ['-e', "require('node:fs').appendFileSync(process.argv[1], 'executed\\n');process.stdout.write('ACTION_OK');", marker],
            readOnly: false, parameters: {}, env: {}, vars: {}, timeoutMs: 10000
        }] : [] }],
        groups: [{ chatId: 'group1', projectId: 'fixture', members: ['alice', 'bob'] }], dms: [], limits: { updateMs: 250 }
    });
    f.transport = {
        send: async (room, text, replyTo, threadId) => { const id = `om_sent_${randomUUID()}`; f.sent.push({ id, room, text, replyTo, threadId }); return id; },
        card: async (room, card, replyTo, threadId) => { const id = `om_card_${randomUUID()}`; f.sent.push({ id, room, card, replyTo, threadId }); return id; },
        update: async (id, card) => { f.updates.push({ id, card }); },
        receipt: async (id, state = 'processing') => { f.receipts.push({ id, state }); },
        download: async (id, resource) => { f.downloads.push({ id, resource }); assert.ok(f.resources.has(resource.key), 'unknown fixture resource'); return f.resources.get(resource.key); },
        fetch: async id => f.quotes.get(id), history: async () => ({ messages: [] }),
        file: async (room, path, name) => { const entry = { room, name, bytes: await readFile(path), done: false }; f.files.push(entry); await f.onFile?.(entry); entry.done = true; return `om_file_${randomUUID()}`; }
    };
    const factory = async (room, session, emit, workspace) => {
        const instance = { sessionId: session.id, resumed: session.ccId, inputs: [], events: [] }; f.workers.push(instance);
        const worker = await runtimeFactory(f.config, [], (event, detail) => f.logs.push({ event, detail }))(room, session, event => {
            f.events.push(event); instance.events.push(event); f.onEvent?.(event); emit(event);
        }, workspace);
        return { send: async delivery => { instance.inputs.push(delivery); await worker.send(delivery); }, respond: (id, value) => worker.respond(id, value), stop: () => worker.stop() };
    };
    const identity = { openId: 'ou_fixture_bot', name: 'Fixture bot' };
    const open = async () => {
        f.store = new Store(join(root, 'gateway.sqlite'));
        f.gateway = new Gateway(f.config, f.store, f.transport, factory);
        // Observe completion only. Never bypass the production SDK safety lane or Gateway entry.
        const handle = f.gateway.message.bind(f.gateway);
        f.gateway.message = async message => {
            try { await handle(message); }
            finally { f.handled.add(message.id); }
        };
        f.channel = feishuTransport(f.config, (event, detail) => f.logs.push({ event, detail })).channel;
        // Test-only ingress: the actual pinned SDK dispatcher/normalizer/safety still run.
        f.channel.botIdentity = identity;
        f.channel.safety.setBotIdentity(identity);
        f.channel.registerDispatcherHandlers();
        f.channel.on('reject', event => f.rejected.set(event.messageId, event.reason));
        f.closed = false;
        bindFeishuInbound(f.channel, f.gateway, (event, detail) => f.logs.push({ event, detail }), () => f.closed);
        await f.gateway.recover();
    };
    f.raw = (type, content, options = {}) => ({
        sender: { sender_id: { open_id: options.author ?? 'alice' }, sender_type: 'user' },
        message: { message_id: options.id ?? `om_input_${randomUUID()}`, chat_id: options.chat ?? 'group1', chat_type: 'group',
            message_type: type, content: JSON.stringify(content), create_time: String(Date.now()), parent_id: options.reply, thread_id: options.thread,
            mentions: options.mention === false ? [] : [{ key: '@_user_1', id: { open_id: identity.openId }, name: identity.name }] }
    });
    f.receive = async event => {
        const id = event.message.message_id;
        await f.channel.dispatcher.handles.get('im.message.receive_v1')(event);
        // pushMessage resolves after enqueue, not after onMessage. Assertions and restart
        // must wait for actual handler completion rather than racing the SDK's async lane.
        await until(() => f.handled.has(id) || f.rejected.has(id), 'raw message handled or explicitly rejected');
        return id;
    };
    f.text = (text, options = {}) => f.receive(f.raw('text', { text: `@_user_1 ${text}` }, options));
    f.click = (card, operator, value) => f.channel.dispatcher.handles.get('card.action.trigger')({
        context: { open_message_id: card.id, open_chat_id: 'group1' }, operator: { open_id: operator }, action: { tag: 'button', value }
    });
    f.status = id => f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(id)?.status;
    f.selected = () => f.store.db.prepare("SELECT id,cc FROM sessions WHERE state='open'").get();
    f.usage = async () => {
        const before = f.requests.length, workers = f.workers.length;
        const id = await f.text('/usage --json');
        const value = JSON.parse(f.sent.find(item => item.replyTo === id)?.text ?? 'null');
        assert.ok(value); assert.equal(f.requests.length, before); assert.equal(f.workers.length, workers);
        return value;
    };
    f.done = id => f.receipts.some(r => r.id === id && r.state === 'done');
    f.restart = async () => {
        f.closed = true; await f.gateway.close(); await f.channel.safety.dispose(); await f.channel.disconnect(); f.store.close();
        await open();
    };
    f.close = async () => {
        f.closed = true;
        try { await f.gateway?.close(); }
        finally {
            await f.channel?.safety.dispose(); await f.channel?.disconnect(); f.store?.close();
            modelServer.closeAllConnections(); await new Promise(resolve => modelServer.close(resolve));
            for (const [index, name] of [modelEnv, appEnv].entries()) { if (previous[index] === undefined) delete process.env[name]; else process.env[name] = previous[index]; }
            await rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
        }
    };
    await open();
    return f;
}
