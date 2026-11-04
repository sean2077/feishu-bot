import test from 'node:test';
import assert from 'node:assert/strict';
import { NativeOutput } from '../src/native-output.ts';
import { nativeResultOrigin } from '../src/native-result-origin.ts';
import { fixture, message, until } from './helpers.ts';

test('ordinary result retains the trusted human completion when raw native stamps contain only metadata', () => {
    const known = new Set(['human']);
    for (const origin of [undefined, { kind: 'human' }]) {
        assert.deepEqual(nativeResultOrigin({ user_message_uuid: 'synthetic', user_message_uuids: ['old', 'synthetic'], origin }, known, 'human'), { uuid: 'human', uuids: ['human'] });
    }
    assert.deepEqual(nativeResultOrigin({ user_message_uuid: 'unknown' }, known, 'unknown'), { uuid: undefined, uuids: [] });
    assert.deepEqual(nativeResultOrigin({ user_message_uuid: 'unknown' }, known), { uuid: undefined, uuids: [] });
    assert.deepEqual(nativeResultOrigin({ user_message_uuid: 'unknown', user_message_uuids: ['human'] }, known, 'other'), { uuid: undefined, uuids: ['human'] }, 'existing raw confirmation takes precedence');
});

test('background, delegated and unknown result origins cannot consume a foreground input through anchor fallback', () => {
    const known = new Set(['human']);
    for (const origin of [{ kind: 'task-notification', runId: 'known-run' }, { kind: 'agent', senderTaskId: 'known-task' }, { kind: 'human', senderTaskId: 'task' }, { kind: 'unknown' }]) {
        assert.deepEqual(nativeResultOrigin({ origin, user_message_uuid: 'synthetic' }, known, 'human'), { uuid: undefined, uuids: [] });
    }
    assert.deepEqual(nativeResultOrigin({ parent_tool_use_id: 'delegated-tool', user_message_uuid: 'synthetic' }, known, 'human'), { uuid: undefined, uuids: [] });
});

test('an unstamped terminal frame retains explicit background classification without losing real folded human consumption', () => {
    const known = new Set(['human']), output = new NativeOutput(() => {}, id => known.has(id));
    output.handle({ type: 'stream_event', user_message_uuid: 'human', event: { type: 'message_start', message: { id: 'm' } } });
    output.handle({ type: 'system', subtype: 'task_started', task_id: 'task', run_id: 'run' });
    output.handle({ type: 'assistant', origin: { kind: 'task-notification', runId: 'run' }, message: { id: 'bg', content: [] } });
    const raw = { type: 'result' };
    output.handle(raw);
    assert.equal(output.origin(raw), 'human');
    assert.equal(output.source(raw), 'background');
    assert.deepEqual(nativeResultOrigin(raw, known, output.origin(raw), output.source(raw)), { uuid: undefined, uuids: [] });
    assert.deepEqual(nativeResultOrigin({ ...raw, user_message_uuid: 'human', user_message_uuids: ['human'] }, known, output.origin(raw), 'background'),
        { uuid: 'human', uuids: ['human'] }, 'raw native consumption confirmation always takes precedence');
});

test('metadata-only terminal stamps complete both gateway turns and never reuse a closed human renderer', async () => {
    const f = await fixture();
    try {
        const known = new Set<string>();
        let worker: (typeof f.workers)[number];
        const output = new NativeOutput(event => worker.emit(event), id => known.has(id));
        for (const index of [1, 2]) {
            const input = message({ text: `Fixture round ${index}` });
            await f.gateway.message(input);
            await until(() => f.workers[0]?.sent.length === index);
            worker = f.workers[0]!;
            const uuid = worker.sent[index - 1]!.uuid;
            known.add(uuid);
            worker.emit({ type: 'consumed', uuid });
            output.handle({ type: 'stream_event', user_message_uuid: uuid, event: { type: 'message_start', message: { id: `m${index}` } } });
            output.handle({ type: 'stream_event', event: { type: 'content_block_delta', delta: { type: 'thinking_delta', thinking: `THINKING_ROUND_${index}` } } });
            const raw = { type: 'result', user_message_uuid: 'synthetic', user_message_uuids: ['synthetic'], origin: { kind: 'human' } };
            output.handle(raw);
            const replyToUuid = output.origin(raw), resultOrigin = nativeResultOrigin(raw, known, replyToUuid);
            worker.emit({ type: 'result', text: `ANSWER_ROUND_${index}`, success: true, ...resultOrigin, replyToUuid });
            for (const id of resultOrigin.uuids) known.delete(id);
            if (resultOrigin.uuid) known.delete(resultOrigin.uuid);
            output.endTurn();
            await until(() => f.store.db.prepare('SELECT state FROM outbox WHERE text=?').get(`ANSWER_ROUND_${index}`)?.state === 'done');
            assert.equal(f.store.db.prepare('SELECT status FROM inbox WHERE id=?').get(input.id)?.status, 'done', 'a synthetic terminal UUID must still finish the confirmed human');
            await until(() => f.transport.receiptStates.some(receipt => receipt.id === input.id && receipt.state === 'done'));
        }
        const replies = f.store.db.prepare("SELECT text,target FROM outbox WHERE kind='reply' AND state='done' ORDER BY at").all();
        assert.deepEqual(replies.map(reply => reply.text), ['ANSWER_ROUND_1', 'ANSWER_ROUND_2']);
        assert.equal(new Set(replies.map(reply => reply.target)).size, 2, 'each foreground turn must have its own live card');
        assert.equal(f.workers.length, 1, 'normal continuation must keep the warm native worker');
        assert.equal(known.size, 0);
    } finally { await f.close(); }
});
