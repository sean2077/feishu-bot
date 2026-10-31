import test from 'node:test';
import assert from 'node:assert/strict';
import { writeFile, mkdir, readFile, symlink } from 'node:fs/promises';
import { join } from 'node:path';
import { Outbox } from '../src/outbox.ts';
import { ActionBroker } from '../src/actions.ts';
import { startActionRelay } from '../src/action-relay.ts';
import { prepareWorkspace } from '../src/workspace.ts';
import { route } from '../src/gateway.ts';
import { fixture, message, until } from './helpers.ts';
test('failed create is uncertain and requires explicit scoped retry', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!, session = f.store.session(room.key);
        const outbox = new Outbox(f.store, f.transport);
        const original = f.transport.card.bind(f.transport);
        f.transport.card = async () => { throw new Error('ambiguous network failure'); };
        const id = outbox.save(room, session.id, 'answer', true);
        assert.equal(await outbox.deliver(id, room), false);
        assert.equal(outbox.pending(room.key)[0]!.state, 'uncertain');
        assert.deepEqual(outbox.retryable(room.key), []);
        f.transport.card = original;
        assert.equal(await outbox.deliver(id, room), false);
        assert.equal(await outbox.deliver(id, route(f.config, message({ chatId: 'group2' }))!, true), false);
        assert.equal(await outbox.deliver(id, room, true), true);
        assert.equal(await outbox.deliver(id, room, true), false);
    }
    finally {
        await f.close();
    }
});
test('known-card updates are retried and concurrent outbox instances claim once', async () => {
    const f = await fixture();
    try {
        const room = route(f.config, message())!, session = f.store.session(room.key);
        const first = new Outbox(f.store, f.transport), second = new Outbox(f.store, f.transport);
        const id = first.save(room, session.id, 'final', true, 'known-card');
        let release: () => void = () => { };
        let calls = 0;
        f.transport.update = async () => { calls++; await new Promise<void>(r => { release = r; }); };
        const sending = first.deliver(id, room);
        await until(() => calls === 1);
        assert.equal(await second.deliver(id, room, true), false);
        release();
        assert.equal(await sending, true);
        assert.equal(calls, 1);
    }
    finally {
        await f.close();
    }
});
test('lazy attachments are accessible only from the current room', async () => {
    const f = await fixture();
    try {
        f.config.limits.backgroundMessages = 30; // Optional cache is explicitly enabled in this regression.
        const resource = { key: 'file-key', kind: 'file' as const, name: '资料.txt' };
        const context = message({ mentioned: false, resources: [resource] });
        await f.gateway.message(context);
        await f.gateway.message(message());
        await f.gateway.message(message({ chatId: 'group2' }));
        await until(() => f.workers.length === 2 && f.workers.every(w => w.sent.length));
        const current = f.workers.find(w => w.room.chatId === 'group1')!, other = f.workers.find(w => w.room.chatId === 'group2')!;
        current.emit({ type: 'request', id: 'file-own', kind: 'attachment', name: 'fetch_attachment', input: { messageId: context.id, key: resource.key } });
        other.emit({ type: 'request', id: 'file-other', kind: 'attachment', name: 'fetch_attachment', input: { messageId: context.id, key: resource.key } });
        await until(() => current.responses.length === 1 && other.responses.length === 1);
        assert.equal(current.responses[0]!.value.ok, true);
        assert.equal(other.responses[0]!.value.ok, false);
        assert.equal(await readFile(current.responses[0]!.value.path as string, 'utf8'), 'attachment');
        assert.ok(!(current.responses[0]!.value.path as string).includes('workspace'));
    }
    finally {
        await f.close();
    }
});
test('approval binds a file snapshot rather than mutable agent-owned bytes', async () => {
    const f = await fixture();
    try {
        const work = join(f.dir, 'work'), stage = join(f.dir, 'private-stage');
        await mkdir(work);
        await writeFile(join(work, 'change.patch'), 'approved-content');
        const room = route(f.config, message())!, session = f.store.session(room.key), controller = new AbortController();
        const broker = new ActionBroker(f.store, f.gateway.approvals, [], stage);
        const spec = { id: 'snapshot', description: 'publish a patch', executable: process.execPath, args: ['-e', 'const fs=require("fs");console.log(fs.readFileSync(process.argv[1],"utf8"))', '{{patch}}'], env: {}, vars: {}, readOnly: false, timeoutMs: 5000, parameters: { patch: { maxLength: 100, workspaceFile: true } } };
        const project = { ...f.config.projects[0]!, actions: [spec] };
        const run = broker.execute(room, session.id, project, 'snapshot', { patch: '/workspace/change.patch' }, controller.signal, work, path => path.replace('/workspace/', work + '/'));
        await until(() => !!f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get());
        const id = f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id as string;
        await writeFile(join(work, 'change.patch'), 'changed-after-request');
        await f.gateway.approvals.click(room.chatId, 'admin', { request: id, decision: 'allow' });
        const result = await run;
        assert.equal(result.ok, true);
        assert.equal((result.output as string).trim(), 'approved-content');
    }
    finally {
        await f.close();
    }
});
test('native Monitor action relay rejects unauthenticated and unconfigured calls', async () => {
    const calls: {
        name: string;
        input: object;
    }[] = [];
    const relay = await startActionRelay(['status'], async (name, input) => { calls.push({ name, input }); return { ok: true, state: 'running' }; });
    try {
        const call = (token: string, action: string) => fetch(relay.url, { method: 'POST', headers: { authorization: 'Bearer ' + token, 'content-type': 'application/json' }, body: JSON.stringify({ action, parameters: {} }) });
        assert.equal((await call('x', 'status')).status, 401);
        assert.equal((await call(relay.token, 'publish-unconfigured')).status, 400);
        const response = await call(relay.token, 'status');
        assert.equal(response.status, 200);
        assert.deepEqual(await response.json(), { ok: true, state: 'running' });
        assert.equal(calls.length, 1);
    }
    finally {
        relay.close();
    }
});
test('seeded workspaces exclude credentials and produce an independent Git baseline', async () => {
    const f = await fixture();
    try {
        const seed = join(f.dir, 'source');
        await mkdir(seed);
        await writeFile(join(seed, 'source.txt'), 'initial');
        await writeFile(join(seed, '.env'), 'PRIVATE_TOKEN=never-copy');
        const project = { ...f.config.projects[0]!, seedDir: seed }, room = route(f.config, message())!, session = f.store.session(room.key);
        const w = await prepareWorkspace(f.config, room, session, project);
        assert.equal(await readFile(join(w.work, 'source.txt'), 'utf8'), 'initial');
        await assert.rejects(readFile(join(w.work, '.env')));
        assert.ok(await readFile(join(w.work, '.git', 'HEAD'), 'utf8'));
        await writeFile(join(w.work, 'source.txt'), 'my work');
        await prepareWorkspace(f.config, room, session, project);
        assert.equal(await readFile(join(w.work, 'source.txt'), 'utf8'), 'my work');
        assert.equal(await readFile(join(seed, 'source.txt'), 'utf8'), 'initial');
    }
    finally {
        await f.close();
    }
});

test('ambiguous preview creation is not retried or duplicated by final delivery', async () => {
    const f = await fixture();
    try {
        let creates = 0;
        const original = f.transport.card.bind(f.transport);
        f.transport.card = async () => { creates++; throw new Error('create may have succeeded'); };
        await f.gateway.message(message());
        await until(() => f.workers.length === 1 && f.workers[0]!.sent.length === 1);
        const worker = f.workers[0]!;
        worker.emit({ type: 'text', text: 'working', partial: true, turnId: worker.sent[0]!.uuid });
        await until(() => creates === 1);
        worker.emit({ type: 'text', text: ' more', partial: true, turnId: worker.sent[0]!.uuid });
        await new Promise(resolve => setTimeout(resolve, 350));
        assert.equal(creates, 1);
        worker.emit({ type: 'result', text: 'final answer', success: true, uuids: [worker.sent[0]!.uuid] });
        await until(() => !!f.store.db.prepare("SELECT id FROM outbox WHERE state='uncertain'").get());
        assert.equal(creates, 1);
        const outbox = new Outbox(f.store, f.transport);
        assert.deepEqual(outbox.retryable(worker.room.key), []);
        f.transport.card = original;
        const id = outbox.pending(worker.room.key)[0]!.id;
        assert.equal(await outbox.deliver(id, worker.room, true), true);
        assert.equal(f.transport.sent.filter(s => s.card).length, 1);
    } finally { await f.close(); }
});

test('agent send_file request snapshots bytes and prevents a false completed turn on upload failure', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const workspace = await prepareWorkspace(f.config, worker.room, worker.session, f.config.projects[0]!);
        const source = join(workspace.work, 'report.txt');
        await writeFile(source, 'original report');
        f.transport.file = async () => { throw new Error('ambiguous file upload'); };
        worker.emit({ type: 'request', id: 'file-1', kind: 'send_file', name: 'send_file', input: { path: source, name: 'report.txt' } });
        await until(() => worker.responses.length === 1);
        const result = worker.responses[0]!.value;
        assert.equal(result.ok, false);
        const id = result.deliveryId as string;
        assert.ok(id, 'failed upload must return a durable recovery ID');
        const file = f.store.db.prepare('SELECT kind,state,file_state,file_path FROM outbox WHERE id=?').get(id)!;
        assert.equal(file.kind, 'file'); assert.equal(file.state, 'uncertain'); assert.equal(file.file_state, 'uncertain');
        await writeFile(source, 'changed later');
        assert.equal(await readFile(file.file_path as string, 'utf8'), 'original report', 'retry uses the snapshot, not mutable workspace bytes');
        worker.emit({ type: 'result', success: true, text: 'The report is ready.', uuids: [worker.sent[0]!.uuid] });
        await until(() => !!f.store.db.prepare("SELECT id FROM outbox WHERE kind='reply' AND state='pending' AND target IS NOT NULL").get());
        assert.ok(!f.transport.receiptStates.some(r => r.state === 'done'));
        assert.equal(f.gateway.outbox.pending(worker.room.key).find(r => r.id === id)?.stage, 'file');
    } finally { await f.close(); }
});
