import test from 'node:test';
import assert from 'node:assert/strict';
import { replyCard } from '../src/reply-card.ts';
import { fixture, message, until } from './helpers.ts';

function footer(card: object): string {
    const elements = (card as { body: { elements: { tag: string; content?: string }[] } }).body.elements;
    return elements.slice(elements.findIndex(element => element.tag === 'hr') + 1).map(element => element.content ?? '').join('\n');
}
test('reply footer distinguishes observed last-request context from aggregate turn usage and uses actual working path', () => {
    const text = footer(replyCard('回复', 'answer', '', true, { elapsedMs: 12345, model: 'fixture-model', projectPath: 'C:/Projects/example',
        context: { inputTokens: 25000, windowTokens: 100000 }, usage: { input_tokens: 900000, output_tokens: 123 } }));
    assert.match(text, /⏱ 用时 12\.3 秒/);
    assert.match(text, /fixture-model/); assert.match(text, /out 123 in 900\.0k/);
    assert.match(text, /ctx 25%（末次输入 25\.0k \/ 100\.0k）/);
    assert.match(text, /C:\/Projects\/example/);
    assert.ok(!text.includes('900%'), 'aggregate input usage must not become context occupancy');
});

test('reply footer reports unavailable context without inferring capacity from native model modifiers or usage', () => {
    const text = footer(replyCard('回复', 'answer', '', true, { elapsedMs: 63000, model: 'fixture[1m]', projectPath: '/srv/example', usage: { input_tokens: 12345 } }));
    assert.match(text, /1 分 03 秒/); assert.match(text, /ctx 未知/);
    assert.ok(!text.includes('1.0M'), 'no native window was reported');
    const streaming = replyCard('处理中', '', 'thinking', false, { elapsedMs: 63000, model: 'fixture', projectPath: '/srv/example' }) as { body: { elements: { tag: string }[] } };
    assert.ok(!streaming.body.elements.some(element => element.tag === 'hr'), 'status footer is emitted at the turn boundary, without a constantly changing streaming clock');
});

test('first attributed thinking creates the same reply card before the ordinary update interval', async () => {
    const f = await fixture();
    try {
        f.config.limits.updateMs = 60000;
        await f.gateway.message(message()); await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!, uuid = worker.sent[0]!.uuid;
        worker.emit({ type: 'thinking', text: 'visible native thought', partial: true, blockId: 'first:0', turnId: uuid });
        await until(() => f.transport.sent.some(item => !!item.card), 1000);
        worker.emit({ type: 'result', success: true, text: 'answer', uuids: [uuid] });
        await until(() => !!f.store.db.prepare("SELECT id FROM outbox WHERE state='done'").get());
        assert.equal(f.transport.sent.filter(item => !!item.card).length, 1, 'final delivery must update the initial card');
    } finally { await f.close(); }
});
