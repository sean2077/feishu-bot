#!/usr/bin/env node
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir, release, availableParallelism } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { performance } from 'node:perf_hooks';
import { setTimeout as delay } from 'node:timers/promises';
import { parseConfig } from '../dist/config.js';
import { Store } from '../dist/store.js';
import { Gateway } from '../dist/gateway.js';
import { runtimeFactory } from '../dist/runtime.js';
import { route } from '../dist/policy.js';
import { distribution, memorySnapshot, reportedUsage } from './benchmark-metrics.mjs';

function options(argv) {
    const result = { samples: 3, warmTurns: 2, idleMs: 1000 };
    const names = { '--samples': ['samples', 1, 10], '--warm-turns': ['warmTurns', 1, 5], '--idle-ms': ['idleMs', 100, 10000] };
    const seen = new Set();
    for (let i = 0; i < argv.length; i++) {
        const spec = names[argv[i]], value = Number(argv[i + 1]);
        if (!spec || seen.has(argv[i]) || !Number.isInteger(value) || value < spec[1] || value > spec[2])
            throw new Error('Use --samples 1..10 --warm-turns 1..5 --idle-ms 100..10000, each at most once.');
        seen.add(argv[i]); result[spec[0]] = value; i++;
    }
    return result;
}
const ms = value => value === null ? null : Math.round(value * 100) / 100;

async function localModel() {
    const counts = { messages: 0, countTokens: 0, rejected: 0 };
    const server = createServer(async (request, response) => {
        try {
            let bytes = 0;
            const chunks = [];
            for await (const chunk of request) {
                bytes += chunk.length;
                if (bytes > 8388608) throw new Error('fixture-input-limit');
                chunks.push(chunk);
            }
            const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
            response.setHeader('content-type', 'application/json');
            if (request.url?.startsWith('/v1/messages/count_tokens')) {
                counts.countTokens++; response.end(JSON.stringify({ input_tokens: 500 })); return;
            }
            if (request.method !== 'POST' || !request.url?.startsWith('/v1/messages')) {
                counts.rejected++; response.writeHead(404); response.end('{}'); return;
            }
            counts.messages++;
            const usage = { input_tokens: 500, output_tokens: 30, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 };
            const message = { id: 'msg_fixture_' + counts.messages, type: 'message', role: 'assistant', model: 'fixture-model',
                content: [{ type: 'text', text: 'BENCHMARK_OK' }], stop_reason: 'end_turn', stop_sequence: null, usage };
            if (!body.stream) { response.end(JSON.stringify(message)); return; }
            response.setHeader('content-type', 'text/event-stream');
            const event = (type, data) => response.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
            event('message_start', { message: { ...message, content: [], stop_reason: null, usage: { ...usage, output_tokens: 0 } } });
            event('content_block_start', { index: 0, content_block: { type: 'text', text: '' } });
            event('content_block_delta', { index: 0, delta: { type: 'text_delta', text: 'BENCHMARK_OK' } });
            event('content_block_stop', { index: 0 });
            event('message_delta', { delta: { stop_reason: 'end_turn', stop_sequence: null }, usage: { output_tokens: 30 } });
            event('message_stop', {}); response.end();
        } catch { counts.rejected++; if (!response.headersSent) response.writeHead(400); response.end(); }
    });
    await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
    return { counts, url: `http://127.0.0.1:${server.address().port}`, close: async () => {
        server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
    } };
}

async function sample(index, settings) {
    const dataDir = await mkdtemp(join(tmpdir(), 'feishu-native-baseline-'));
    const model = await localModel();
    const failures = [];
    const mark = stage => { if (!failures.includes(stage)) failures.push(stage); };
    const result = { index, status: 'failed', failures, serviceInitMs: null, workerStartupMs: null,
        workersStarted: 0, sessionStable: true, turns: [], idle: null, stopMs: null, cleanupConfirmed: false,
        modelRequests: model.counts };
    let store, gateway, current, nativeId;
    const start = performance.now();
    try {
        const config = parseConfig({ tenantId: 'tenant_fixture', appId: 'cli_fixture', dataDir,
            appSecretEnv: 'FEISHU_BASELINE_UNUSED_APP_SECRET', runtime: { maxWorkers: 1, idleMs: 0 },
            model: { baseUrl: model.url, allowHttp: true, tokenEnv: 'FEISHU_BASELINE_FIXTURE_TOKEN', model: 'fixture-model' },
            projects: [{ id: 'baseline', maintainers: ['alice'], permissionMode: 'default', actions: [] }],
            groups: [{ chatId: 'fixture-chat', projectId: 'baseline', members: ['alice'] }], dms: [],
            limits: { backgroundMessages: 0, updateMs: 250 } });
        store = new Store(join(dataDir, 'gateway.sqlite'));
        const transport = {
            async send() { return randomUUID(); }, async card() { return randomUUID(); }, async update() {},
            async receipt(id, state) { if (state === 'done' && current?.id === id) current.doneAt ??= performance.now(); },
            async file() { throw new Error('fixture-does-not-upload'); }, async fetch() { return undefined; },
            async history() { return { messages: [] }; }, async download() { throw new Error('fixture-does-not-download'); }
        };
        const native = runtimeFactory(config, [], (event, detail) => {
            if (event === 'worker_ready') result.workerStartupMs = detail.startupMs;
            if (event === 'worker_descendant_cleanup_failed') mark('descendant-cleanup');
        });
        gateway = new Gateway(config, store, transport, async (room, session, emit, workspace) => {
            result.workersStarted++;
            return native(room, session, event => {
                const now = performance.now();
                if (event.type === 'ready') {
                    if (nativeId && event.ccId !== nativeId) { result.sessionStable = false; mark('session-changed'); }
                    nativeId = event.ccId;
                }
                if (current && (event.type === 'text' || event.type === 'thinking') && event.text) current.firstAt ??= now;
                if (current && event.type === 'result') {
                    current.resultAt = now; current.success = event.success; current.usage = reportedUsage(event.usage);
                    if (!event.success) mark('native-result');
                }
                if (event.type === 'error') mark('native-error');
                emit(event);
            }, workspace);
        });
        await gateway.recover(); result.serviceInitMs = ms(performance.now() - start);
        let savedSession;
        for (let turn = 0; turn <= settings.warmTurns; turn++) {
            const id = randomUUID();
            current = { id, started: performance.now(), firstAt: null, resultAt: null, doneAt: null, success: false, usage: reportedUsage() };
            const input = { id, chatId: 'fixture-chat', chatType: 'group', senderId: 'alice', text: 'Reply BENCHMARK_OK without using tools.',
                resources: [], time: Date.now(), mentioned: true };
            await gateway.message(input);
            while (current.doneAt === null && !failures.length && performance.now() - current.started < 20000) await delay(10);
            if (current.doneAt === null) mark('turn-not-delivered');
            if (current.firstAt === null) mark('first-output-not-observed');
            const session = store.session(route(config, input).key);
            if (!session.ccId || (savedSession && (savedSession.id !== session.id || savedSession.ccId !== session.ccId))) {
                result.sessionStable = false; mark('session-not-preserved');
            }
            savedSession = session;
            result.turns.push({ kind: turn === 0 ? 'cold-native' : 'warm-native', index: turn, success: current.success && current.doneAt !== null,
                firstOutputMs: current.firstAt === null ? null : ms(current.firstAt - current.started),
                nativeResultMs: current.resultAt === null ? null : ms(current.resultAt - current.started),
                mockDeliveryMs: current.doneAt === null ? null : ms(current.doneAt - current.started), usage: current.usage });
            if (failures.length) break;
        }
        current = undefined;
        if (result.workersStarted !== 1) mark('unexpected-worker-count');
        const cpu = process.cpuUsage(), idleStart = performance.now();
        await delay(settings.idleMs);
        const cpuUsed = process.cpuUsage(cpu);
        result.idle = { elapsedMs: ms(performance.now() - idleStart), hostCpuMs: ms((cpuUsed.user + cpuUsed.system) / 1000),
            memory: await memorySnapshot() };
        if (model.counts.rejected) mark('fixture-protocol');
    } catch { mark('sample-exception'); }
    finally {
        const stopping = performance.now();
        try { await gateway?.close(); result.cleanupConfirmed = true; }
        catch { mark('shutdown-unconfirmed'); }
        result.stopMs = ms(performance.now() - stopping);
        try { store?.close(); } catch { mark('store-close'); }
        await model.close();
        if (result.cleanupConfirmed) await rm(dataDir, { recursive: true, force: true });
    }
    result.status = failures.length ? 'failed' : 'passed';
    return result;
}

async function main() {
    const settings = options(process.argv.slice(2));
    const previous = process.env.FEISHU_BASELINE_FIXTURE_TOKEN;
    process.env.FEISHU_BASELINE_FIXTURE_TOKEN = 'local-fixture-not-a-real-credential';
    try {
        const lock = JSON.parse(await readFile(new URL('../package-lock.json', import.meta.url), 'utf8'));
        let revision = null;
        try { revision = execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8', windowsHide: true }).trim(); } catch {}
        const report = { schema: 1, fixture: 'deterministic-text-v1', startedAt: new Date().toISOString(),
            environment: { platform: process.platform, release: release(), arch: process.arch, node: process.version,
                logicalCpus: availableParallelism(), revision, sdk: lock.packages['node_modules/@anthropic-ai/claude-agent-sdk'].version },
            settings, scope: {
                model: 'Local deterministic HTTP fixture; reported token values are synthetic, not billing or context size.',
                transport: 'In-memory Feishu substitute; mockDeliveryMs is not network delivery latency.',
                cold: 'Fresh native process/session/workspace per sample, not a cold OS or cold module/filesystem cache.',
                memory: 'Host includes benchmark, Gateway, SQLite and fixture HTTP server. Descendants include visible worker/CC processes. RSS/working-set sums can double-count shared pages, not unique physical RAM.',
                collection: 'One post-idle read-only snapshot, excluded from turn timings; observer processes excluded. Failed collection is unavailable, never zero.',
                liveServiceAccepted: false, cost: null, contextOccupancy: null }, samples: [] };
        for (let i = 0; i < settings.samples; i++) report.samples.push(await sample(i + 1, settings));
        const cold = report.samples.flatMap(s => s.turns.filter(t => t.kind === 'cold-native'));
        const warm = report.samples.flatMap(s => s.turns.filter(t => t.kind === 'warm-native'));
        const passed = report.samples.filter(s => s.status === 'passed').length;
        report.summary = { attemptedSamples: settings.samples, passedSamples: passed, failedSamples: settings.samples - passed,
            attemptedTurns: cold.length + warm.length, failedTurns: [...cold, ...warm].filter(t => !t.success).length,
            coldFirstOutputMs: distribution(cold.filter(t => t.success).map(t => t.firstOutputMs)),
            warmFirstOutputMs: distribution(warm.filter(t => t.success).map(t => t.firstOutputMs)),
            stopMs: distribution(report.samples.filter(s => s.cleanupConfirmed).map(s => s.stopMs)) };
        console.log(JSON.stringify(report, null, 2));
        if (passed !== settings.samples) process.exitCode = 1;
    } finally {
        if (previous === undefined) delete process.env.FEISHU_BASELINE_FIXTURE_TOKEN;
        else process.env.FEISHU_BASELINE_FIXTURE_TOKEN = previous;
    }
}
main().catch(() => { console.error('Native baseline could not complete; check arguments, build, dependencies and temporary-directory access.'); process.exitCode = 1; });
