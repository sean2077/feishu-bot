import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import { parseConfig } from '../dist/config.js';

// Production runtime launcher and a real Node child process. The process-tree
// module is explicitly replaced to inject cleanup failure on both platforms;
// this is not proof of an actual Windows CIM error or real CC execution.
test('runtime reports unexpected child cleanup failure and rejects later stop', { timeout: 30000 }, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-unconfirmed-exit-'));
    const originalToken = process.env.NATIVE_CLEANUP_FIXTURE_TOKEN;
    process.env.NATIVE_CLEANUP_FIXTURE_TOKEN = 'fixture-only-token';
    let worker;
    try {
        const workerPath = join(dir, 'worker.js');
        await writeFile(workerPath, [
            "process.stdin.once('data', () => setTimeout(() => process.exit(17), 40));",
            "process.stdin.resume();"
        ].join('\n'));
        const processPath = join(dir, 'process-mock.mjs');
        await writeFile(processPath, [
            'export function trackProcessTree() {}',
            "export async function killTree() { throw new Error('fixture descendant cleanup failure'); }",
            'export class JsonLines { constructor() {} push() {} }'
        ].join('\n'));
        const originalUrl = new URL('../dist/runtime.js', import.meta.url);
        const source = await readFile(originalUrl, 'utf8');
        assert.match(source, /from ['"]\.\/process\.js['"]/);
        const patched = source.replace(/(from\s+)(['"])(\.\/[^'"]+)\2/g, (_match, from, quote, specifier) =>
            from + quote + (specifier === './process.js' ? pathToFileURL(processPath) : new URL(specifier, originalUrl)).href + quote);
        const runtimePath = join(dir, 'runtime.mjs');
        await writeFile(runtimePath, patched);
        const { runtimeFactory } = await import(pathToFileURL(runtimePath).href);
        const config = parseConfig({
            tenantId: 'tenant_fixture', appId: 'cli_fixture', dataDir: dir,
            model: { baseUrl: 'https://model.example.org/anthropic', model: 'fixture-model', tokenEnv: 'NATIVE_CLEANUP_FIXTURE_TOKEN' },
            projects: [{ id: 'p', maintainers: ['admin'], actions: [] }], groups: [], dms: []
        });
        const room = { key: 'fixture:group1', chatId: 'group1', projectId: 'p', maintainers: ['admin'] };
        const session = { id: randomUUID(), roomKey: room.key, state: 'open', createdAt: Date.now() };
        const workspace = { root: dir, work: dir, home: dir, inbound: dir };
        const logs = [], events = [];
        let resolveExit;
        const exit = new Promise(resolve => { resolveExit = resolve; });
        worker = await runtimeFactory(config, [], (name, detail) => logs.push({ name, detail }))(
            room, session, event => {
                events.push(event);
                if (event.type === 'exit') resolveExit(event);
            }, workspace
        );
        const exited = await Promise.race([
            exit, new Promise((_resolve, reject) => setTimeout(() => reject(new Error('exit event missing')), 10000))
        ]);
        assert.equal(exited.code, 17);
        assert.equal(exited.cleanupConfirmed, false,
            'an ordinary exit would release the session while descendants might survive');
        assert.ok(logs.some(item => item.name === 'worker_descendant_cleanup_failed'));
        assert.equal(events.filter(item => item.type === 'exit').length, 1);
        await assert.rejects(worker.stop(), /fixture descendant cleanup failure|termination unconfirmed/i,
            'subsequent stop cannot erase a previously unconfirmed cleanup');
    } finally {
        if (worker) await worker.stop().catch(() => {});
        if (originalToken === undefined) delete process.env.NATIVE_CLEANUP_FIXTURE_TOKEN;
        else process.env.NATIVE_CLEANUP_FIXTURE_TOKEN = originalToken;
        await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
});
