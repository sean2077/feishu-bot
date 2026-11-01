import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { join } from 'node:path';

const root = fileURLToPath(new URL('../', import.meta.url));
const cliUrl = new URL('../dist/cli.js', import.meta.url);
// The built CLI, gateway, SQLite and data lock are real. Feishu and Worker are
// explicit local substitutes; process.emit tests the signal handler, not OS delivery.
const channelFixture = `
import { EventEmitter } from 'node:events';
import { randomUUID } from 'node:crypto';
export const normalize = message => message;
export function feishuTransport() {
    const channel = new EventEmitter();
    const keepAlive = setInterval(() => {}, 1000);
    channel.connect = async () => {
        const message = { id: 'fixture-message', chatId: 'group1', chatType: 'group',
            senderId: 'alice', text: 'fixture', time: Date.now(), mentioned: true, resources: [] };
        for (const listener of channel.listeners('message')) await listener(message);
    };
    channel.disconnect = async () => { clearInterval(keepAlive); };
    const transport = {
        send: async () => randomUUID(), card: async () => randomUUID(), update: async () => {},
        receipt: async () => {}, file: async () => randomUUID(), fetch: async () => undefined,
        history: async () => ({ messages: [] }), download: async () => new Uint8Array()
    };
    return { channel, transport };
}
`;
const runtimeFixture = `
import { appendFileSync } from 'node:fs';
import { join } from 'node:path';
export function runtimeFactory(config) {
    return async (_room, _session, emit) => ({
        send: async delivery => {
            emit({ type: 'accepted', uuid: delivery.uuid });
            setImmediate(() => process.emit('SIGTERM'));
        },
        respond: () => {},
        stop: async () => {
            appendFileSync(join(config.dataDir, 'fixture-stop.log'), 'stop\\n');
            if (process.env.FEISHU_FIXTURE_FAIL_STOP === '1')
                throw new Error('fixture stop failure: ' + process.env.FEISHU_SHUTDOWN_FIXTURE_SECRET);
        }
    });
}
`;
async function run(entry, configPath, env) {
    const child = spawn(process.execPath, [entry, 'run', '--config', configPath], {
        stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true, env
    });
    let stdout = '', stderr = '', expired = false;
    child.stdout.setEncoding('utf8'); child.stdout.on('data', value => { stdout += value; });
    child.stderr.setEncoding('utf8'); child.stderr.on('data', value => { stderr += value; });
    const timer = setTimeout(() => { expired = true; child.kill('SIGKILL'); }, 15000);
    try {
        const result = await new Promise((resolve, reject) => {
            child.once('error', reject);
            child.once('close', (code, signal) => resolve({ code, signal }));
        });
        assert.equal(expired, false, 'fixture CLI did not exit after its shutdown signal: ' + stderr);
        return { ...result, pid: child.pid, stdout, stderr };
    } finally { clearTimeout(timer); }
}

for (const fail of [false, true]) {
    test(`service CLI ${fail ? 'retains lock and fails' : 'releases lock'} after native shutdown`, { timeout: 40000 }, async () => {
        const dir = await mkdtemp(join(root, '.cli-shutdown-'));
        try {
            const channelPath = join(dir, 'channel.mjs'), runtimePath = join(dir, 'runtime.mjs');
            await writeFile(channelPath, channelFixture);
            await writeFile(runtimePath, runtimeFixture);
            const source = await readFile(cliUrl, 'utf8');
            assert.match(source, /import\(['"]\.\/feishu\.js['"]\)/);
            assert.match(source, /import\(['"]\.\/runtime\.js['"]\)/);
            const patched = source.replace(/(from\s+|import\()(['"])(\.\/[^'"]+)\2/g, (_match, prefix, quote, specifier) => {
                const url = specifier === './feishu.js' ? pathToFileURL(channelPath)
                    : specifier === './runtime.js' ? pathToFileURL(runtimePath) : new URL(specifier, cliUrl);
                return prefix + quote + url.href + quote;
            });
            const entry = join(dir, 'cli.mjs'), configPath = join(dir, 'config.json'), dataDir = join(dir, 'data');
            await writeFile(entry, patched);
            await writeFile(configPath, JSON.stringify({
                tenantId: 'tenant_fixture', appId: 'cli_fixture', dataDir,
                appSecretEnv: 'FEISHU_SHUTDOWN_FIXTURE_SECRET',
                model: { baseUrl: 'https://model.fixture.test/anthropic', model: 'fixture-model', tokenEnv: 'FEISHU_SHUTDOWN_FIXTURE_MODEL' },
                projects: [{ id: 'p', maintainers: ['admin'], actions: [] }],
                groups: [{ chatId: 'group1', projectId: 'p', members: ['alice'] }], dms: []
            }));
            const secret = 'fixture-credential-never-log';
            const env = { ...process.env, FEISHU_SHUTDOWN_FIXTURE_SECRET: secret,
                FEISHU_SHUTDOWN_FIXTURE_MODEL: secret, FEISHU_FIXTURE_FAIL_STOP: fail ? '1' : '0' };
            const result = await run(entry, configPath, env);
            assert.equal(result.code, fail ? 1 : 0, result.stderr);
            assert.equal(result.signal, null, 'successful test path must not force-kill the service');
            assert.equal((result.stdout + result.stderr).includes(secret), false, 'credentials must not escape through shutdown diagnostics');
            const lock = join(dataDir, 'gateway.lock');
            if (fail) {
                assert.match(result.stderr, /shutdown_failed/);
                assert.match(result.stderr, /termination unconfirmed/i);
                const before = await readFile(lock, 'utf8');
                assert.equal(JSON.parse(before).pid, result.pid, 'retain the original owner for explicit investigation');
                const second = await run(entry, configPath, env);
                assert.equal(second.code, 1, second.stderr);
                assert.match(second.stderr, /Data directory is locked/);
                assert.equal(await readFile(lock, 'utf8'), before, 'restart cannot steal the unconfirmed lock');
            } else {
                await assert.rejects(readFile(lock), { code: 'ENOENT' });
                assert.doesNotMatch(result.stderr, /shutdown_failed/);
            }
            assert.equal(await readFile(join(dataDir, 'fixture-stop.log'), 'utf8'), 'stop\n',
                'failed restart must not create another worker or replay its input');
        } finally { await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }); }
    });
}
