import test from 'node:test';
import assert from 'node:assert/strict';
import { migrationFixture, until, deferred } from './migration-fixture.mjs';

const uuidFor = (f, id) => f.store.db.prepare('SELECT uuid FROM inbox WHERE id=?').get(id)?.uuid;
const completes = (event, uuid) => event.type === 'result' && (event.uuid === uuid || event.uuids?.includes(uuid));

test('a supplement arriving after the actual native result cannot start another turn while gateway observation is delayed', { timeout: 90000 }, async t => {
    const firstResponse = deferred();
    const f = await migrationFixture(async (_f, request) => {
        if (request.number === 1) await firstResponse.promise;
        return { text: `HANDOFF_REPLY_${request.number}` };
    });
    // Delay only host observation of actual worker frames; retain their FIFO order.
    // The real CC/SDK, native input pipe, Gateway admission and raw SDK ingress still run.
    const forward = f.gateway.event.bind(f.gateway);
    const pending = [];
    let hold = false;
    f.gateway.event = (slot, event) => {
        if (hold) pending.push({ slot, event });
        else forward(slot, event);
    };
    const flush = () => {
        hold = false;
        for (const { slot, event } of pending.splice(0)) forward(slot, event);
    };
    try {
        const initial = await f.text('HANDOFF_START');
        await until(() => f.requests.length === 1, 'first actual model request');
        const selected = { ...f.selected() }, initialUuid = uuidFor(f, initial);
        assert.ok(selected.cc);
        hold = true;
        firstResponse.resolve();
        await until(() => f.events.some(event => completes(event, initialUuid)), 'actual native task has ended');
        assert.equal(f.done(initial), false, 'gateway has not yet observed the native result');

        const supplement = await f.text('/btw MUST_NOT_BECOME_A_NEW_TASK', { author: 'bob' });
        const uuid = uuidFor(f, supplement);
        assert.ok(uuid, 'the stale host view admitted a persisted supplement');
        await until(() => f.events.some(event => event.uuid === uuid && event.type === 'input_rejected' || completes(event, uuid)),
            'actual worker rejects the late supplement or exposes an unwanted extra result');
        flush();
        await until(() => f.done(initial) && ['cancelled', 'done'].includes(f.status(supplement)), 'attributed terminal states');

        assert.equal(f.requests.length, 1, 'a lost-target supplement must make no second model request');
        assert.ok(f.events.some(event => event.type === 'input_rejected' && event.uuid === uuid && event.reason === 'foreground_ended'));
        assert.ok(!f.events.some(event => event.type === 'accepted' && event.uuid === uuid));
        assert.equal(f.status(supplement), 'cancelled');
        assert.equal(f.done(supplement), false);
        assert.equal(f.workers.length, 1);
        assert.deepEqual({ ...f.selected() }, selected);
        assert.equal(f.sent.filter(item => item.card?.header?.title?.content === '回复').length, 1);
        await until(() => f.sent.some(item => item.replyTo === supplement && JSON.stringify(item.card).includes('补充已取消')), 'durable cancellation feedback');
        assert.ok(!f.sent.some(item => item.replyTo === supplement && item.text?.includes('补充已交给原生输入流')));
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM usage_results').get().n, 1);
        await f.restart();
        assert.equal(f.requests.length, 1, 'restart cannot replay the rejected input');
        assert.equal(f.status(supplement), 'cancelled');
        assert.deepEqual({ ...f.selected() }, selected);
        assert.deepEqual(f.errors, []);
        t.diagnostic('Real CC/SDK with deterministic model and simulated Feishu: native target ended before supplement ingress; delayed host observation preserves FIFO; no second inference, new session or false Done.');
    } finally {
        firstResponse.resolve();
        flush();
        await f.close();
    }
});
