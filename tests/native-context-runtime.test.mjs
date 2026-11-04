import test from 'node:test';
import assert from 'node:assert/strict';
import { migrationFixture, until, fields, sumUsage } from './migration-fixture.mjs';

test('real native context metadata reports the last request input rather than tool-round or warm-session totals', { timeout: 90000 }, async t => {
    let historyRequested = false;
    const f = await migrationFixture((_f, request) => {
        if (!historyRequested) { historyRequested = true; return { content: [{ type: 'tool_use', id: 'toolu_context_history', name: 'mcp__gateway__read_chat_history', input: { limit: 1 } }] }; }
        return { text: `CONTEXT_FIXTURE_${request.number}` };
    });
    try {
        for (const round of [1, 2]) {
            const start = f.requests.length;
            const id = await f.text(`Context fixture round ${round}`);
            await until(() => f.done(id), 'context fixture final delivery');
            const inputUuid = f.store.db.prepare('SELECT uuid FROM inbox WHERE id=?').get(id).uuid;
            const result = f.events.find(event => event.type === 'result' && (event.uuid === inputUuid || event.uuids?.includes(inputUuid)));
            assert.ok(result?.context, 'production worker must forward context metadata');
            const last = f.requests.at(-1).usage;
            assert.equal(result.context.inputTokens, last.input_tokens + last.cache_read_input_tokens + last.cache_creation_input_tokens);
            assert.deepEqual(Object.fromEntries(fields.map(field => [field, result.usage[field]])), sumUsage(f.requests.slice(start)), 'existing per-turn accounting remains separate');
            if (round === 1) {
                assert.equal(f.requests.length - start, 2, 'the first turn includes a real MCP tool round');
                assert.notEqual(result.context.inputTokens, result.usage.input_tokens + result.usage.cache_read_input_tokens + result.usage.cache_creation_input_tokens);
            }
            t.diagnostic(JSON.stringify({ round, inputTokens: result.context.inputTokens, nativeWindow: result.context.windowTokens ?? 'unknown' }));
        }
        assert.equal(f.workers.length, 1, 'both rounds use one warm native session');
        assert.deepEqual(f.errors, []);
    } finally { await f.close(); }
});
