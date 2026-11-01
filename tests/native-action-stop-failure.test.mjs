import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';

// Built action runner and a real Node process, with process-tree cleanup
// deliberately replaced. This is not a Windows CIM or external-action test.
test('action cancellation exposes unconfirmed descendant termination', { timeout: 20000 }, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-action-stop-'));
    try {
        const stub = join(dir, 'process-mock.mjs');
        await writeFile(stub, [
            'export function trackProcessTree() {}',
            'export async function killTree(child) {',
            "    child.kill('SIGKILL');",
            '    await new Promise(resolve => setTimeout(resolve, 40));',
            "    throw new Error('fixture cleanup failure');",
            '}'
        ].join('\n'));
        const sourceUrl = new URL('../dist/actions.js', import.meta.url);
        const source = await readFile(sourceUrl, 'utf8');
        assert.match(source, /from ['"]\.\/process\.js['"]/);
        const patched = source.replace(/(from\s+)(['"])(\.\/[^'"]+)\2/g, (_match, prefix, quote, specifier) =>
            prefix + quote + (specifier === './process.js' ? pathToFileURL(stub) : new URL(specifier, sourceUrl)).href + quote);
        const target = join(dir, 'actions.mjs');
        await writeFile(target, patched);
        const { runCommand } = await import(pathToFileURL(target).href);
        const controller = new AbortController();
        const running = runCommand(process.execPath, ['-e', 'setInterval(() => {}, 1000)'],
            { signal: controller.signal, timeoutMs: 10000 });
        void running.catch(() => {});
        await delay(150);
        controller.abort();
        await assert.rejects(running, /termination unconfirmed/i,
            'a cleanup failure cannot be reported as ordinary confirmed cancellation');
    } finally {
        await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
});
