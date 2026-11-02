import test from 'node:test';
import assert from 'node:assert/strict';
import { distribution, memoryFromRows, reportedUsage } from '../scripts/benchmark-metrics.mjs';

test('baseline token reporting keeps unknown fields distinct from reported zero', () => {
    assert.deepEqual(reportedUsage({ input_tokens: 0, output_tokens: 30, cache_read_input_tokens: -1 }), {
        input_tokens: 0, output_tokens: 30, cache_read_input_tokens: null, cache_creation_input_tokens: null
    });
    assert.ok(Object.values(reportedUsage()).every(value => value === null));
    assert.ok(Object.values(reportedUsage({ input_tokens: '500', output_tokens: Infinity })).every(value => value === null));
});

test('baseline snapshot includes descendants but excludes the observer and unrelated processes', () => {
    const rows = [
        { pid: 1, ppid: 0, bytes: 10 }, { pid: 2, ppid: 1, bytes: 20 }, { pid: 3, ppid: 2, bytes: 30 },
        { pid: 4, ppid: 1, bytes: 40 }, { pid: 5, ppid: 4, bytes: 50 }, { pid: 6, ppid: 0, bytes: 60 }
    ];
    assert.deepEqual(memoryFromRows(rows, 1, 4), { hostBytes: 10, descendantBytes: 50, descendantCount: 2 });
    assert.throws(() => memoryFromRows(rows, 99, 4), /root-not-observed/);
    assert.throws(() => memoryFromRows([{ pid: 1, ppid: 0, bytes: NaN }], 1, 4), /invalid-memory/);
});

test('baseline distributions disclose the sample count and never turn missing observations into zero', () => {
    assert.deepEqual(distribution([null, 8, 2, 6, 4]), { count: 4, min: 2, median: 5, max: 8 });
    assert.deepEqual(distribution([undefined, NaN]), { count: 0, min: null, median: null, max: null });
});
