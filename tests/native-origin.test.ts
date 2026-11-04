import test from 'node:test';
import assert from 'node:assert/strict';
import { NativeOutput } from '../src/native-output.ts';
import type { Args, WorkerEvent } from '../src/types.ts';
import { fixture, message, until } from './helpers.ts';
import { nativeResultOrigin } from '../src/native-result-origin.ts';

test('native mixed prompt stamps select a registered human and preserve it across synthetic metadata', () => {
    const events: WorkerEvent[] = [], known = new Set(['human']);
    const output = new NativeOutput(event => events.push(event), id => known.has(id));
    output.handle({ type: 'stream_event', user_message_uuid: 'synthetic', user_message_uuids: ['synthetic', 'human'], event: { type: 'message_start', message: { id: 'm' } } });
    output.handle({ type: 'stream_event', user_message_uuid: 'skill-meta', event: { type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: 'public reasoning' } } });
    output.handle({ type: 'assistant', user_message_uuid: 'skill-meta', message: { id: 'm', content: [{ type: 'thinking', thinking: 'public reasoning' }] } });
    assert.ok(events.every(event => (event as Args).turnId === 'human'));
    assert.equal(output.origin({ type: 'result', user_message_uuid: 'skill-meta' }), 'human');
    output.endTurn();
    output.handle({ type: 'stream_event', user_message_uuid: 'unknown', event: { type: 'content_block_delta', delta: { type: 'text_delta', text: 'unbound' } } });
    assert.equal((events.at(-1) as Args).turnId, undefined, 'unknown initial metadata is not an authorization to choose the queued human');
});

test('unknown background origins and their unstamped continuations never borrow the latest human', () => {
    const events: WorkerEvent[] = [];
    const output = new NativeOutput(event => events.push(event), id => id === 'human');
    output.handle({ type: 'stream_event', user_message_uuid: 'human', event: { type: 'message_start', message: { id: 'm' } } });
    output.handle({ type: 'system', subtype: 'task_started', task_id: 'task', run_id: 'run' });
    assert.equal(output.origin({ origin: { kind: 'task-notification', runId: 'run' } }), 'human');
    output.handle({ type: 'stream_event', origin: { kind: 'task-notification', runId: 'unknown' }, user_message_uuid: 'human', event: { type: 'message_start', message: { id: 'unknown-background' } } });
    output.handle({ type: 'stream_event', event: { type: 'content_block_delta', delta: { type: 'text_delta', text: 'unknown background' } } });
    assert.equal((events.at(-1) as Args).turnId, undefined);
    output.endTurn();
    output.handle({ type: 'system', subtype: 'task_started', task_id: 'unrelated-task', run_id: 'unrelated-run' });
    assert.equal(output.origin({ origin: { kind: 'task-notification', runId: 'unrelated-run' } }), undefined, 'a root task started outside a known turn has no inferred owner');
    assert.equal(output.origin({ origin: { kind: 'task-notification', runId: 'run' } }), 'human', 'a previously registered task keeps its own owner');
});

test('explicit unknown background thinking is never adopted by the next confirmed human result', async () => {
    const f = await fixture();
    try {
        const input = message(); await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        const output = new NativeOutput(event => worker.emit(event), id => id === uuid);
        output.handle({ type: 'stream_event', origin: { kind: 'task-notification', runId: 'UNKNOWN_RUN' },
            event: { type: 'message_start', message: { id: 'unknown-background' } } });
        output.handle({ type: 'stream_event', event: { type: 'content_block_delta', index: 0,
            delta: { type: 'thinking_delta', thinking: 'UNOWNED_BACKGROUND_REASONING' } } });
        worker.emit({ type: 'result', uuid, uuids: [uuid], replyToUuid: uuid, text: 'FOREGROUND_ANSWER', success: true });
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('FOREGROUND_ANSWER')?.state === 'done');
        const human = f.store.db.prepare('SELECT card,reply_to,origins FROM outbox WHERE text=?').get('FOREGROUND_ANSWER')!;
        assert.equal(String(human.card).includes('UNOWNED_BACKGROUND_REASONING'), false);
        assert.equal(human.reply_to, input.id);
        assert.equal(JSON.parse(String(human.origins)).length, 1);
    } finally { await f.close(); }
});

test('known background task renders separately and cannot finish or mark Done its still-active human input', async () => {
    const f = await fixture();
    try {
        const input = message(); await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        const output = new NativeOutput(event => worker.emit(event), id => id === uuid);
        output.handle({ type: 'stream_event', user_message_uuid: uuid, event: { type: 'message_start', message: { id: 'human' } } });
        output.handle({ type: 'system', subtype: 'task_started', task_id: 'known-task', run_id: 'known-run' });
        output.handle({ type: 'assistant', origin: { kind: 'task-notification', runId: 'known-run' }, message: { id: 'bg', content: [{ type: 'thinking', thinking: 'KNOWN_BACKGROUND_ONLY' }] } });
        const raw = { type: 'result', origin: { kind: 'task-notification', runId: 'known-run' } };
        output.handle(raw);
        worker.emit({ type: 'result', uuids: [], replyToUuid: output.origin(raw), source: output.source(raw), backgroundId: output.backgroundId(raw), text: 'BACKGROUND_RESULT', success: true });
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('BACKGROUND_RESULT')?.state === 'done');
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)!.status, 'sent');
        assert.equal(f.transport.receiptStates.some(item => item.id === input.id && item.state === 'done'), false);
        assert.equal(JSON.parse(String(f.store.db.prepare('SELECT origins FROM outbox WHERE text=?').get('BACKGROUND_RESULT')!.origins)).length, 0);
        worker.emit({ type: 'result', uuid, uuids: [uuid], replyToUuid: uuid, source: 'foreground', text: 'HUMAN_RESULT', success: true });
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('HUMAN_RESULT')?.state === 'done');
        assert.equal(String(f.store.db.prepare('SELECT card FROM outbox WHERE text=?').get('HUMAN_RESULT')!.card).includes('KNOWN_BACKGROUND_ONLY'), false);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)!.status, 'done');
    } finally { await f.close(); }
});

test('native explicit consumed human UUIDs complete a human folded into a background turn without borrowing its preview', async () => {
    const f = await fixture();
    try {
        const input = message(); await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        const output = new NativeOutput(event => worker.emit(event), id => id === uuid);
        output.handle({ type: 'assistant', origin: { kind: 'task-notification', runId: 'unknown-run' },
            message: { id: 'scheduled', content: [{ type: 'thinking', thinking: 'SCHEDULED_PREVIEW' }] } });
        worker.emit({ type: 'result', source: 'background', uuid, uuids: [uuid], text: 'FOLDED_HUMAN_ANSWER', success: true });
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('FOLDED_HUMAN_ANSWER')?.state === 'done');
        const row = f.store.db.prepare('SELECT card,origins,reply_to FROM outbox WHERE text=?').get('FOLDED_HUMAN_ANSWER')!;
        assert.equal(String(row.card).includes('SCHEDULED_PREVIEW'), false);
        assert.equal(row.reply_to, input.id);
        assert.equal(JSON.parse(String(row.origins)).length, 1);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)!.status, 'done');
    } finally { await f.close(); }
});

test('unattributed foreground preview still binds when a confirmed human result arrives later', async () => {
    const f = await fixture();
    try {
        const input = message(); await f.gateway.message(input);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        const output = new NativeOutput(event => worker.emit(event), id => id === uuid);
        output.handle({ type: 'assistant', message: { id: 'late', content: [{ type: 'thinking', thinking: 'CANDIDATE_FOREGROUND' }] } });
        worker.emit({ type: 'result', uuid, uuids: [uuid], text: 'LATE_BOUND_HUMAN', success: true });
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('LATE_BOUND_HUMAN')?.state === 'done');
        const result = f.store.db.prepare('SELECT card,reply_to FROM outbox WHERE text=?').get('LATE_BOUND_HUMAN')!;
        assert.equal(String(result.card).includes('CANDIDATE_FOREGROUND'), true);
        assert.equal(result.reply_to, input.id);
    } finally { await f.close(); }
});

test('known background turn that consumes a later human finalizes its existing card with honest original reply ownership', async () => {
    const f = await fixture();
    try {
        const first = message({ text: 'First fixture input' }); await f.gateway.message(first);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, a = worker.sent[0]!.uuid, known = new Set([a]);
        const output = new NativeOutput(event => worker.emit(event), id => known.has(id));
        output.handle({ type: 'stream_event', user_message_uuid: a, event: { type: 'message_start', message: { id: 'first' } } });
        output.handle({ type: 'system', subtype: 'task_started', task_id: 'owned-task', run_id: 'owned-run' });
        worker.emit({ type: 'result', uuid: a, uuids: [a], replyToUuid: a, text: 'FIRST_COMPLETE', success: true });
        known.delete(a); output.endTurn();
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('FIRST_COMPLETE')?.state === 'done');
        const second = message({ text: 'Later fixture input' }); await f.gateway.message(second);
        await until(() => worker.sent.length === 2);
        const b = worker.sent[1]!.uuid; known.add(b);
        output.handle({ type: 'assistant', origin: { kind: 'task-notification', runId: 'owned-run' },
            message: { id: 'owned-background', content: [{ type: 'thinking', thinking: 'OWNED_BACKGROUND_THINKING' }] } });
        await until(() => f.transport.sent.some(item => item.replyTo === first.id && JSON.stringify(item.card).includes('OWNED_BACKGROUND_THINKING')));
        const preview = f.transport.sent.find(item => JSON.stringify(item.card).includes('OWNED_BACKGROUND_THINKING'))!;
        const sentBeforeFinal = f.transport.sent.length;
        const raw = { type: 'result', origin: { kind: 'task-notification', runId: 'owned-run' }, user_message_uuid: b, user_message_uuids: [b] };
        output.handle(raw);
        worker.emit({ type: 'result', ...nativeResultOrigin(raw, known, output.origin(raw), output.source(raw)),
            source: output.source(raw), backgroundId: output.backgroundId(raw), replyToUuid: output.origin(raw), text: 'FOLDED_LATER_ANSWER', success: true });
        await until(() => !!f.store.db.prepare("SELECT 1 FROM outbox WHERE text LIKE '%FOLDED_LATER_ANSWER%' AND state='done'").get());
        const row = f.store.db.prepare("SELECT card,target,reply_to,origins FROM outbox WHERE text LIKE '%FOLDED_LATER_ANSWER%'").get()!;
        assert.equal(row.target, preview.id, 'the existing known-background card must become the final card');
        assert.equal(row.reply_to, first.id, 'the platform reply target cannot be rewritten after message creation');
        assert.equal(f.transport.sent.length, sentBeforeFinal, 'do not leave an extra streaming preview behind');
        assert.deepEqual(JSON.parse(String(row.origins)).map((item: { id: string }) => item.id), [second.id]);
        assert.equal(JSON.parse(String(row.card)).config.streaming_mode, false);
        assert.match(String(row.card), /卡片仍引用/);
        assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(second.id)!.status, 'done');
    } finally { await f.close(); }
});

test('a folded known background excerpt also closes when the later human already has its own preview', async () => {
    const f = await fixture();
    try {
        const first = message(); await f.gateway.message(first);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, a = worker.sent[0]!.uuid;
        worker.emit({ type: 'result', uuid: a, text: 'PRIOR_COMPLETE', success: true });
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('PRIOR_COMPLETE')?.state === 'done');
        const second = message(); await f.gateway.message(second);
        await until(() => worker.sent.length === 2);
        const b = worker.sent[1]!.uuid;
        worker.emit({ type: 'thinking', text: 'CURRENT_HUMAN_PREVIEW', partial: false, turnId: b, source: 'foreground' });
        worker.emit({ type: 'thinking', text: 'OLD_BACKGROUND_PREVIEW', partial: false, turnId: a, source: 'background', backgroundId: 'old-run' });
        await until(() => f.transport.sent.some(item => JSON.stringify(item.card).includes('CURRENT_HUMAN_PREVIEW')) &&
            f.transport.sent.some(item => JSON.stringify(item.card).includes('OLD_BACKGROUND_PREVIEW')));
        const human = f.transport.sent.find(item => JSON.stringify(item.card).includes('CURRENT_HUMAN_PREVIEW'))!;
        const background = f.transport.sent.find(item => JSON.stringify(item.card).includes('OLD_BACKGROUND_PREVIEW'))!;
        const count = f.transport.sent.length;
        worker.emit({ type: 'result', uuid: b, uuids: [b], replyToUuid: a, source: 'background', backgroundId: 'old-run', text: 'LATER_COMPLETE', success: true });
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('LATER_COMPLETE')?.state === 'done' &&
            !!f.store.db.prepare('SELECT 1 FROM outbox WHERE target=? AND state=?').get(background.id, 'done'));
        const bgFinal = f.store.db.prepare('SELECT card,reply_to,origins FROM outbox WHERE target=?').get(background.id)!;
        assert.equal(JSON.parse(String(bgFinal.card)).config.streaming_mode, false);
        assert.equal(bgFinal.reply_to, first.id);
        assert.deepEqual(JSON.parse(String(bgFinal.origins)), []);
        const humanFinal = f.store.db.prepare('SELECT target,reply_to,origins FROM outbox WHERE text=?').get('LATER_COMPLETE')!;
        assert.equal(humanFinal.target, human.id); assert.equal(humanFinal.reply_to, second.id);
        assert.deepEqual(JSON.parse(String(humanFinal.origins)).map((item: { id: string }) => item.id), [second.id]);
        assert.equal(f.transport.sent.length, count);
    } finally { await f.close(); }
});

test('native user background triggers classify unstamped assistant frames without resetting on tool-result user frames', () => {
    const events: WorkerEvent[] = [], known = new Set(['human']);
    const output = new NativeOutput(event => events.push(event), id => known.has(id));
    output.handle({ type: 'user', uuid: 'human', origin: { kind: 'human' }, message: { role: 'user', content: 'synthetic fixture' } });
    output.handle({ type: 'system', subtype: 'task_started', task_id: 'task', run_id: 'run' });
    output.endTurn();
    output.handle({ type: 'user', origin: { kind: 'task-notification', runId: 'run', senderTaskId: 'task' }, message: { role: 'user', content: 'background fixture' } });
    output.handle({ type: 'user', uuid: 'tool-result-metadata', message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tool-fixture', content: 'fixture result' }] } });
    output.handle({ type: 'assistant', message: { id: 'bg', content: [{ type: 'thinking', thinking: 'BACKGROUND_FROM_USER_ORIGIN' }] } });
    const emitted = events.at(-1)! as Extract<WorkerEvent, { type: 'text' | 'thinking' }>;
    assert.equal(emitted.source, 'background'); assert.equal(emitted.backgroundId, 'run'); assert.equal(emitted.turnId, 'human');
    output.handle({ type: 'result', origin: { kind: 'task-notification', runId: 'run' } });
    assert.equal(output.source({}), 'background'); assert.equal(output.backgroundId({}), 'run');
    output.endTurn();
    assert.equal(output.source({}), 'unattributed'); assert.equal(output.backgroundId({}), undefined);
    output.handle({ type: 'user', origin: { kind: 'agent', senderTaskId: 'task' }, message: { role: 'user', content: 'fixture' } });
    assert.equal(output.backgroundId({}), 'task', 'senderTaskId is the fallback when no runId is present');
    output.handle({ type: 'user', origin: { kind: 'unknown' }, message: { role: 'user', content: 'fixture' } });
    assert.equal(output.source({}), 'background'); assert.equal(output.backgroundId({}), undefined, 'missing background identity is never guessed');
});

test('dual-ID background task history stays bounded and preserves recent alias ownership across turns', () => {
    const events: WorkerEvent[] = [];
    const output = new NativeOutput(event => events.push(event), id => id === 'human-one' || id === 'human-two');
    const anchors = (output as unknown as { taskAnchors: Map<string, string> }).taskAnchors;
    for (let i = 0; i < 10010; i++) {
        output.handle({ type: 'user', uuid: i < 10009 ? 'human-one' : 'human-two', origin: { kind: 'human' } });
        output.handle({ type: 'system', subtype: 'task_started', task_id: `task-${i}`, run_id: `run-${i}` });
        assert.ok(anchors.size <= 10000, `task aliases exceeded the bound at event ${i}: ${anchors.size}`);
        output.endTurn();
    }
    assert.equal(anchors.size, 10000);
    assert.equal(anchors.has('task-0'), false);
    assert.equal(anchors.has('run-0'), false);
    for (const [origin, human, identity] of [
        [{ kind: 'task-notification', runId: 'run-10008' }, 'human-one', 'run-10008'],
        [{ kind: 'agent', senderTaskId: 'task-10009' }, 'human-two', 'task-10009'],
        [{ kind: 'task-notification', runId: 'run-10009' }, 'human-two', 'run-10009'],
    ] as const) {
        output.handle({ type: 'user', origin });
        output.handle({ type: 'assistant', message: { id: identity, content: [{ type: 'text', text: identity }] } });
        const event = events.at(-1)! as Extract<WorkerEvent, { type: 'text' | 'thinking' }>;
        assert.equal(event.turnId, human);
        assert.equal(event.source, 'background');
        assert.equal(event.backgroundId, identity);
        output.endTurn();
    }
    output.handle({ type: 'user', origin: { kind: 'task-notification', runId: 'run-0' } });
    assert.equal(output.origin({}), undefined, 'evicted tasks cannot borrow the last foreground anchor');
});

test('background runs with the same human anchor cannot borrow each other preview when a new human is folded', async () => {
    const f = await fixture();
    try {
        const first = message(); await f.gateway.message(first);
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, a = worker.sent[0]!.uuid, known = new Set([a]);
        const output = new NativeOutput(event => worker.emit(event), id => known.has(id));
        output.handle({ type: 'user', uuid: a, origin: { kind: 'human' } });
        for (const run of ['run-one', 'run-two']) output.handle({ type: 'system', subtype: 'task_started', task_id: run, run_id: run });
        worker.emit({ type: 'result', uuid: a, text: 'INITIAL_DONE', success: true });
        known.delete(a); output.endTurn();
        await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get('INITIAL_DONE')?.state === 'done');
        const second = message(); await f.gateway.message(second);
        await until(() => worker.sent.length === 2);
        const b = worker.sent[1]!.uuid; known.add(b);
        for (const run of ['run-one', 'run-two']) {
            output.handle({ type: 'user', origin: { kind: 'task-notification', runId: run } });
            output.handle({ type: 'assistant', message: { id: run, content: [{ type: 'thinking', thinking: `THINKING_${run}` }] } });
        }
        await until(() => f.transport.sent.some(item => JSON.stringify(item.card).includes('THINKING_run-one')) && f.transport.sent.some(item => JSON.stringify(item.card).includes('THINKING_run-two')));
        const one = f.transport.sent.find(item => JSON.stringify(item.card).includes('THINKING_run-one'))!;
        const two = f.transport.sent.find(item => JSON.stringify(item.card).includes('THINKING_run-two'))!;
        const raw = { type: 'result', origin: { kind: 'task-notification', runId: 'run-two' }, user_message_uuid: b, user_message_uuids: [b] };
        output.handle(raw);
        worker.emit({ type: 'result', ...nativeResultOrigin(raw, known, output.origin(raw), output.source(raw)), source: output.source(raw),
            backgroundId: output.backgroundId(raw), replyToUuid: output.origin(raw), text: 'ONLY_RUN_TWO_FINAL', success: true });
        await until(() => !!f.store.db.prepare("SELECT 1 FROM outbox WHERE text LIKE '%ONLY_RUN_TWO_FINAL%' AND state='done'").get());
        const final = f.store.db.prepare("SELECT card,target FROM outbox WHERE text LIKE '%ONLY_RUN_TWO_FINAL%'").get()!;
        assert.equal(final.target, two.id); assert.notEqual(final.target, one.id);
        assert.equal(String(final.card).includes('THINKING_run-two'), true);
        assert.equal(String(final.card).includes('THINKING_run-one'), false);
        assert.equal(f.store.db.prepare('SELECT 1 FROM outbox WHERE target=?').get(one.id), undefined, 'other live run is not finalized by this run result');
    } finally { await f.close(); }
});
