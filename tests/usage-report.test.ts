import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { join } from 'node:path';
import { rm } from 'node:fs/promises';
import { fixture, message, until } from './helpers.ts';
import { Gateway, route } from '../src/gateway.ts';
import { Store } from '../src/store.ts';
import type { Args, Delivery, WorkerEvent } from '../src/types.ts';

const empty = { input_tokens: null, output_tokens: null, cache_read_input_tokens: null, cache_creation_input_tokens: null };
async function view(f: Awaited<ReturnType<typeof fixture>>, json = true, overrides = {}) {
    const m = message({ text: json ? '/usage --json' : '/usage', ...overrides });
    await f.gateway.message(m);
    const output = f.transport.sent.find(s => s.replyTo === m.id)?.text;
    assert.ok(output, '/usage must reply without model work');
    return json ? JSON.parse(output) : output;
}
function result(uuid: string, usage: Args | undefined, models: string[] | undefined, resultId = randomUUID()): WorkerEvent {
    return { type: 'result', text: 'fixture reply', success: true, uuid, usage, models, resultId } as WorkerEvent;
}
async function begin(f: Awaited<ReturnType<typeof fixture>>, overrides = {}) {
    const m = message(overrides); await f.gateway.message(m);
    await until(() => f.workers.some(w => w.sent.some(d => d.message.id === m.id)));
    const worker = f.workers.find(w => w.sent.some(d => d.message.id === m.id))!;
    return { worker, delivery: worker.sent.find(d => d.message.id === m.id)! };
}

test('usage is read-only, explicitly unknown without results, authorized and documented in help', async () => {
    const f = await fixture();
    try {
        const report = await view(f);
        assert.equal(report.session, null); assert.equal(report.since, null); assert.equal(report.latest, null);
        assert.equal(report.cumulative.results, 0); assert.deepEqual(report.cumulative.usage, empty);
        assert.equal(report.cost, null); assert.equal(report.contextOccupancy, null);
        assert.match(await view(f, false), /尚无/);
        const help = message({ text: '/help' }); await f.gateway.message(help);
        assert.match(f.transport.sent.find(s => s.replyTo === help.id)!.text!, /\/usage/);
        const bad = message({ text: '/usage --other' }); await f.gateway.message(bad);
        assert.match(f.transport.sent.find(s => s.replyTo === bad.id)!.text!, /用法/);
        const before = f.transport.sent.length;
        for (const bad of [{ senderId: 'stranger' }, { mentioned: false }, { chatId: 'foreign' }])
            await f.gateway.message(message({ text: '/usage --json', ...bad }));
        assert.equal(f.transport.sent.length, before);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM sessions').get()!.n, 0);
        assert.equal(f.workers.length, 0); assert.equal(f.transport.receipts.length, 0);
    } finally { await f.close(); }
});

test('usage counts stable result IDs once, distinguishes missing values and ignores cumulative modelUsage/cost', async () => {
    const f = await fixture();
    try {
        const one = await begin(f), id = randomUUID();
        const first = { ...result(one.delivery.uuid, { input_tokens: 100, output_tokens: 17, cache_read_input_tokens: 0, cache_creation_input_tokens: 8 }, ['fixture-main'], id),
            modelUsage: { 'fixture-main': { inputTokens: 999999, costUSD: 999 } }, total_cost_usd: 999 } as WorkerEvent;
        one.worker.emit(first);
        await until(() => f.store.countPending(one.worker.room.key) === 0);
        const initial = await view(f);
        assert.deepEqual(initial.latest.usage, { input_tokens: 100, output_tokens: 17, cache_read_input_tokens: 0, cache_creation_input_tokens: 8 });
        assert.deepEqual(initial.latest.models, ['fixture-main']);
        assert.equal(initial.historyIncomplete, false); assert.equal(initial.unidentifiedResults, false);
        assert.ok(initial.since <= initial.latest.at);
        assert.equal(initial.latest.resultId, id);
        const cardCount = f.transport.sent.filter(s => s.card).length;
        one.worker.emit(first);
        await new Promise(r => setTimeout(r, 20));
        assert.equal(f.transport.sent.filter(s => s.card).length, cardCount, 'duplicate result must not create an extra answer');
        assert.equal((await view(f)).cumulative.results, 1);
        const two = await begin(f);
        two.worker.emit({ ...result(two.delivery.uuid, { input_tokens: 200, cache_read_input_tokens: 4 }, undefined), success: false });
        await until(() => f.store.countPending(one.worker.room.key) === 0);
        const final = await view(f);
        assert.equal(final.cumulative.results, 2);
        assert.deepEqual(final.latest.usage, { input_tokens: 200, output_tokens: null, cache_read_input_tokens: 4, cache_creation_input_tokens: null });
        assert.equal(final.latest.models, null);
        assert.deepEqual(final.cumulative.usage, { input_tokens: 300, output_tokens: null, cache_read_input_tokens: 4, cache_creation_input_tokens: null });
        assert.equal(final.cumulative.reportedSubtotal.output_tokens, 17);
        assert.equal(final.cumulative.missing.output_tokens, 1);
        assert.equal(final.cost, null); assert.equal(final.contextOccupancy, null);
        const human = await view(f, false);
        assert.match(human, /未知/); assert.match(human, /主循环/); assert.match(human, /上下文/); assert.match(human, /不含子代理/);
        assert.ok(!JSON.stringify(final).includes('999999'));
        assert.equal(f.workers[0]!.sent.length, 2, 'querying usage cannot add model requests');
    } finally { await f.close(); }
});

test('usage scope follows the selected conversation, not other chats or the recent-history display window', async () => {
    const f = await fixture();
    try {
        const one = await begin(f);
        one.worker.emit(result(one.delivery.uuid, { input_tokens: 7, output_tokens: 9 }, ['model-a']));
        await until(() => f.store.countPending(one.worker.room.key) === 0);
        const other = await begin(f, { chatId: 'group2' });
        other.worker.emit(result(other.delivery.uuid, { input_tokens: 8000 }, ['FOREIGN_MODEL']));
        await until(() => f.store.countPending(other.worker.room.key) === 0);
        assert.equal((await view(f)).cumulative.usage.input_tokens, 7);
        assert.ok(!JSON.stringify(await view(f)).includes('FOREIGN_MODEL'));
        await f.gateway.message(message({ senderId: 'admin', text: '/stop' }));
        assert.equal((await view(f)).session, one.worker.session.id);
        assert.equal((await view(f)).cumulative.results, 1);
        await f.gateway.message(message({ senderId: 'admin', text: '/new' }));
        const fresh = await view(f); assert.equal(fresh.cumulative.results, 0); assert.equal(fresh.latest, null);
        for (let i = 0; i < 22; i++) f.store.newSession(one.worker.room.key);
        await f.gateway.message(message({ senderId: 'admin', text: `/resume ${one.worker.session.id}` }));
        assert.equal((await view(f)).cumulative.usage.input_tokens, 7);
        assert.equal(f.workers.length, 2);
    } finally { await f.close(); }
});

test('a usage read cannot dispatch an existing ordinary queued task', async () => {
    const f = await fixture();
    try {
        const m = message(), room = route(f.config, m)!, session = f.store.session(room.key);
        const d: Delivery = { uuid: randomUUID(), roomKey: room.key, sessionId: session.id, message: m, background: [], attachments: [] };
        f.store.insert(room.key, m, 'queued'); f.store.queue(d);
        const before = f.store.pending(room.key);
        assert.equal((await view(f)).cumulative.results, 0);
        await new Promise(r => setTimeout(r, 25));
        assert.deepEqual(f.store.pending(room.key), before);
        assert.equal(f.workers.length, 0);
    } finally { await f.close(); }
});

test('invalid numeric usage is unknown, and results without stable identity are not counted as complete totals', async () => {
    const f = await fixture();
    try {
        const one = await begin(f);
        one.worker.emit(result(one.delivery.uuid, { input_tokens: '30', output_tokens: -4, cache_read_input_tokens: true, cache_creation_input_tokens: Number.MAX_SAFE_INTEGER + 1 }, ['fixture-main']));
        await until(() => f.store.countPending(one.worker.room.key) === 0);
        assert.deepEqual((await view(f)).latest.usage, empty);
        const two = await begin(f);
        two.worker.emit({ type: 'result', text: 'unidentified', success: true, uuid: two.delivery.uuid, usage: { input_tokens: 1000 } });
        await until(() => f.store.countPending(one.worker.room.key) === 0);
        const report = await view(f);
        assert.equal(report.unidentifiedResults, true); assert.equal(report.cumulative.results, 1);
        assert.deepEqual(report.cumulative.usage, empty);
        assert.match(await view(f, false), /未纳入/);
    } finally { await f.close(); }
});

test('usage survives database reopen, deduplicates the same native result, and does not backfill legacy audit as complete history', async () => {
    const f = await fixture();
    await f.gateway.close();
    const room = route(f.config, message())!, legacy = f.store.session(room.key);
    f.store.audit(room.key, 'turn_result', { session: legacy.id, usage: { input_tokens: 9999 } });
    // Reproduce a schema-v4 database with historical audit but no usage ledger.
    f.store.db.exec('DROP TABLE IF EXISTS usage_results; DROP TABLE IF EXISTS usage_coverage; PRAGMA user_version=4');
    f.store.close();
    let store = new Store(join(f.dir, 'state.sqlite'));
    let gateway = new Gateway(f.config, store, f.transport, f.factory);
    const read = async () => {
        const m = message({ text: '/usage --json' }); await gateway.message(m);
        return JSON.parse(f.transport.sent.find(s => s.replyTo === m.id)!.text!);
    };
    try {
        await gateway.recover();
        assert.equal((await read()).historyIncomplete, true);
        assert.equal((await read()).cumulative.results, 0);
        const m = message(); await gateway.message(m); await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!, event = result(w.sent[0]!.uuid, { input_tokens: 11, output_tokens: 13 }, ['fixture-main']);
        w.emit(event); await until(() => store.countPending(room.key) === 0);
        const before = await read(); assert.equal(before.cumulative.usage.input_tokens, 11);
        await gateway.close(); store.close();
        store = new Store(join(f.dir, 'state.sqlite')); gateway = new Gateway(f.config, store, f.transport, f.factory);
        await gateway.recover();
        assert.deepEqual(await read(), before);
        const second = message(); await gateway.message(second); await until(() => f.workers[1]?.sent.length === 1);
        f.workers[1]!.emit(event);
        assert.equal((await read()).cumulative.results, 1);
        assert.equal(store.db.prepare('SELECT count(*) AS n FROM usage_results').get()!.n, 1);
        assert.equal(store.db.prepare("SELECT count(*) AS n FROM audit WHERE event='turn_result' AND json_extract(detail,'$.resultId') IS NOT NULL").get()!.n, 1);
    } finally {
        await gateway.close(); store.close(); await rm(f.dir, { recursive: true, force: true });
    }
});
