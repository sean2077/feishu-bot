import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

// Real worker process, fault-injected SDK. This is not a real CC/provider test.
// Keep the fixture under the repository so its unchanged zod import resolves.
const root = fileURLToPath(new URL('../', import.meta.url));
const workerUrl = new URL('../dist/worker.js', import.meta.url);
const fixtureSdk = `
import { appendFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
export const tool = (...args) => ({ name: args[0] });
export const createSdkMcpServer = () => ({});
export function query({ options }) {
    const record = text => appendFileSync(join(options.cwd, 'lifecycle.log'), text + '\\n');
    let finish;
    const ended = new Promise(resolve => { finish = resolve; });
    let beat = 0;
    const heartbeat = setInterval(() => writeFileSync(join(options.cwd, 'background.beat'), String(++beat)), 20);
    const stream = (async function* () {
        yield { type: 'system', subtype: 'init', session_id: options.resume };
        yield { type: 'system', subtype: 'background_tasks_changed', tasks: [{ id: 'fixture-background' }] };
        void options.canUseTool('Bash', { command: 'fixture-only' }, { signal: options.abortController.signal })
            .then(answer => record('permission:' + answer.behavior));
        await ended;
    })();
    stream.interrupt = () => { record('interrupt'); return new Promise(() => {}); };
    stream.close = () => { record('close'); clearInterval(heartbeat); finish(); };
    return stream;
}
`;

async function until(check, message, timeout = 5000) {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
        if (await check()) return;
        await delay(20);
    }
    assert.fail(message);
}
async function within(promise, ms, message) {
    let timer;
    try {
        return await Promise.race([promise, new Promise((_, reject) => {
            timer = setTimeout(() => reject(new Error(message)), ms);
        })]);
    } finally { clearTimeout(timer); }
}

for (const mode of ['host EOF', 'stop frame']) {
    test(`worker ${mode} closes a background query without waiting for a stalled interrupt`, { timeout: 20000 }, async () => {
        const dir = await mkdtemp(join(root, '.worker-shutdown-'));
        let child;
        let closed;
        try {
            const work = join(dir, 'work'), home = join(dir, 'home');
            await Promise.all([mkdir(work), mkdir(home)]);
            const sdkPath = join(dir, 'sdk-fixture.mjs');
            await writeFile(sdkPath, fixtureSdk);
            const source = await readFile(workerUrl, 'utf8');
            assert.match(source, /from ['"]@anthropic-ai\/claude-agent-sdk['"]/);
            // Only substitute the SDK boundary; keep the built worker and its
            // relative imports identical to the production build.
            const instrumented = source
                .replace(/from (['"])@anthropic-ai\/claude-agent-sdk\1/, `from '${pathToFileURL(sdkPath).href}'`)
                .replace(/from (['"])(\.\/[^'"]+)\1/g, (_, quote, specifier) => `from ${quote}${new URL(specifier, workerUrl).href}${quote}`);
            const entry = join(dir, 'worker.mjs');
            await writeFile(entry, instrumented);
            child = spawn(process.execPath, [entry], { cwd: work, stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
            closed = new Promise((resolve, reject) => {
                child.once('error', reject);
                child.once('close', (code, signal) => resolve({ code, signal }));
            });
            void closed.catch(() => {});
            const events = [];
            let buffer = '', stderr = '';
            child.stdout.setEncoding('utf8');
            child.stdout.on('data', chunk => {
                buffer += chunk;
                for (;;) {
                    const end = buffer.indexOf('\n');
                    if (end < 0) break;
                    const line = buffer.slice(0, end); buffer = buffer.slice(end + 1);
                    if (line) events.push(JSON.parse(line));
                }
            });
            child.stderr.setEncoding('utf8');
            child.stderr.on('data', chunk => { stderr += chunk; });
            child.stdin.on('error', () => {});
            const ccId = '11111111-1111-4111-8111-111111111111';
            child.stdin.write(JSON.stringify({ type: 'init', sessionId: 'fixture-session', ccId,
                workDir: work, homeDir: home, permissionMode: 'acceptEdits', allowedTools: [], queueLimit: 8,
                actions: [{ id: 'fixture-action', description: 'Fixture only', parameters: {}, readOnly: true }],
                model: { url: 'https://model.example/v1', token: 'fixture-token', model: 'fixture', smallModel: 'fixture' }
            }) + '\n');
            await until(() => {
                if (child.exitCode !== null || child.signalCode !== null) assert.fail('worker exited before setup: ' + stderr);
                return events.some(e => e.type === 'request' && e.kind === 'permission') &&
                    events.some(e => e.type === 'activity' && e.background === 1);
            }, 'background work and pending permission were not established');
            await until(async () => Number(await readFile(join(work, 'background.beat'), 'utf8').catch(() => '0')) > 1,
                'background fixture was not live');
            assert.deepEqual(events.filter(e => e.type === 'ready').map(e => e.ccId), [ccId]);
            if (mode === 'host EOF') child.stdin.end();
            else child.stdin.write('{"type":"stop"}\n{"type":"stop"}\n');
            // A liveness watchdog, not a startup/performance benchmark. No parent
            // process-tree termination participates in the successful path.
            const result = await within(closed, 5000, 'worker never closed after shutdown; SDK interrupt may be blocking close');
            assert.deepEqual(result, { code: 0, signal: null }, stderr);
            const lifecycle = (await readFile(join(work, 'lifecycle.log'), 'utf8')).trim().split('\n');
            assert.equal(lifecycle.filter(line => line === 'close').length, 1, 'native transport must be closed exactly once');
            assert.ok(lifecycle.includes('permission:deny'), 'pending native permission must be denied');
            assert.equal(events.some(e => e.type === 'result' || e.type === 'error'), false, 'shutdown is not success or a worker failure');
            const lastBeat = await readFile(join(work, 'background.beat'), 'utf8');
            await delay(100);
            assert.equal(await readFile(join(work, 'background.beat'), 'utf8'), lastBeat);
        } finally {
            if (child && child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
            if (closed) await within(closed, 5000, 'fixture worker could not be reaped').catch(() => {});
            await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
        }
    });
}
