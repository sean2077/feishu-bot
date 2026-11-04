import test from 'node:test';
import assert from 'node:assert/strict';
import { NativeContextSnapshot } from '../src/native-context.ts';

test('context uses only the latest main request input and its exact model native window', () => {
    const snapshot = new NativeContextSnapshot();
    snapshot.observe({ type: 'assistant', message: { id: 'first', model: 'fixture-main', usage: { input_tokens: 100, cache_read_input_tokens: 20, cache_creation_input_tokens: 5, output_tokens: 999 } } });
    snapshot.observe({ type: 'stream_event', event: { type: 'message_start', message: { id: 'last', model: 'fixture-main', usage: { input_tokens: 40, cache_read_input_tokens: 30, cache_creation_input_tokens: 10 } } } });
    snapshot.observe({ type: 'assistant', message: { id: 'last', model: 'fixture-main', usage: { input_tokens: 0, output_tokens: 500 } } });
    snapshot.observe({ type: 'assistant', parent_tool_use_id: 'subagent', message: { id: 'child', model: 'fixture-child', usage: { input_tokens: 999999 } } });
    const result = { usage: { input_tokens: 9000, cache_read_input_tokens: 8000 }, modelUsage: { 'fixture-main': { contextWindow: 321000, inputTokens: 99999999 }, 'fixture-child': { contextWindow: 777777 } } };
    assert.deepEqual(snapshot.result(result), { inputTokens: 80, windowTokens: 321000 });
    snapshot.reset();
    assert.equal(snapshot.result(result), undefined, 'a turn without an observed main request cannot inherit stale context');
});

test('footer model follows the last request rather than first-seen distinct model order', () => {
    const snapshot = new NativeContextSnapshot();
    for (const [id, model] of [['first', 'fixture-a'], ['retry', 'fixture-b'], ['restored', 'fixture-a']])
        snapshot.observe({ type: 'assistant', message: { id, model, usage: { input_tokens: 100 } } });
    assert.equal(snapshot.modelName, 'fixture-a');
    assert.deepEqual(snapshot.result({ modelUsage: { 'fixture-a': { contextWindow: 1000 }, 'fixture-b': { contextWindow: 2000 } } }), { inputTokens: 100, windowTokens: 1000 });
});

test('native selector case variants select the same reported row and conflicting variants stay unknown', () => {
    const snapshot = new NativeContextSnapshot();
    snapshot.observe({ type: 'assistant', message: { id: 'last', model: 'fixture-main', usage: { input_tokens: 42 } } });
    const rows = { 'fixture-main': { contextWindow: 200000 }, 'fixture-main[1m]': { contextWindow: 321000 } };
    assert.deepEqual(snapshot.result({ modelUsage: rows }, 'fixture-main[1M]'), { inputTokens: 42, windowTokens: 321000 });
    assert.deepEqual(snapshot.result({ modelUsage: { ...rows, 'fixture-main[1M]': { contextWindow: 654000 } } }, 'fixture-main[1M]'), { inputTokens: 42 });
});

test('unknown or invalid context data is not inferred from cumulative usage or model names', () => {
    const snapshot = new NativeContextSnapshot();
    snapshot.observe({ type: 'assistant', message: { id: 'missing', model: 'fixture[1m]', usage: {} } });
    assert.equal(snapshot.result({ usage: { input_tokens: 42 }, modelUsage: { other: { contextWindow: 1000000 } } }), undefined);
    for (const value of [null, -1, Infinity, '100']) {
        snapshot.observe({ type: 'stream_event', event: { type: 'message_start', message: { id: 'invalid', model: 'fixture-main', usage: { input_tokens: 10, cache_read_input_tokens: value } } } });
        assert.equal(snapshot.result({ modelUsage: { 'fixture-main': { contextWindow: value } } }), undefined);
    }
    snapshot.observe({ type: 'assistant', message: { id: 'valid', model: 'fixture-main', usage: { input_tokens: 12 } } });
    assert.deepEqual(snapshot.result({}), { inputTokens: 12 }, 'optional omitted cache counters add no reported cache input');
    snapshot.observe({ type: 'system', subtype: 'compact_boundary' });
    assert.equal(snapshot.result({}), undefined, 'a compaction boundary invalidates the previous request input');
});

test('native context selectors identify the reported capacity without guessing a window from the suffix', () => {
    const snapshot = new NativeContextSnapshot();
    snapshot.observe({ type: 'assistant', message: { id: 'last', model: 'fixture-main', usage: { input_tokens: 23 } } });
    const rows = { modelUsage: { 'fixture-main': { contextWindow: 200000 }, 'fixture-main[1m]': { contextWindow: 321000 }, 'fixture-child[1m]': { contextWindow: 999999 } } };
    assert.deepEqual(snapshot.result(rows, 'fixture-main[1m]'), { inputTokens: 23, windowTokens: 321000 }, 'the selected native row wins over a plain-name row');
    assert.deepEqual(snapshot.result(rows), { inputTokens: 23, windowTokens: 200000 }, 'exact response metadata wins without a selected decorated request');
    assert.deepEqual(snapshot.result({ modelUsage: { 'fixture-main[1M]': { contextWindow: 321000 }, 'fixture-child[1m]': { contextWindow: 999999 } } }), { inputTokens: 23, windowTokens: 321000 }, 'a unique recognized suffix match is sufficient');
    assert.deepEqual(snapshot.result({ modelUsage: { 'fixture-main[1m]': { contextWindow: 321000 }, 'fixture-main[1M]': { contextWindow: 456000 } } }), { inputTokens: 23 }, 'conflicting normalized keys have no inferred winner');
    assert.deepEqual(snapshot.result({ modelUsage: { 'other-alias[1m]': { contextWindow: 321000 } } }, 'other-alias[1m]'), { inputTokens: 23 }, 'an unrelated request alias is not the response model');
    assert.deepEqual(snapshot.result({ modelUsage: { 'fixture-main[custom]': { contextWindow: 321000 } } }, 'fixture-main[custom]'), { inputTokens: 23 }, 'unknown suffixes are not normalized');
    assert.deepEqual(snapshot.result({ modelUsage: { 'fixture-main[1m]': {} } }, 'fixture-main[1m]'), { inputTokens: 23 }, 'the selector itself supplies no capacity');
});
