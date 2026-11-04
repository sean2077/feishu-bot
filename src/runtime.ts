import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { join, dirname } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import type { Config, Room, Session, Worker, WorkerEvent, WorkerFactory, Workspace } from './types.ts';
import { cleanEnv } from './actions.ts';
import { modelHeaders } from './config.ts';
import { JsonLines, killTree, trackProcessTree } from './process.ts';
import { prepareWorkspace } from './workspace.ts';
import { hash, redact, Serial } from './util.ts';

/** Isolated sessions allowlist their environment; inherited sessions drop bridge/parent controls. */
export function nativeEnv(w: Workspace, appSecretEnv = 'FEISHU_APP_SECRET'): NodeJS.ProcessEnv {
    const env = w.nativeConfig === 'inherit' ? { ...process.env } : cleanEnv();
    for (const key of Object.keys(env)) {
        if (key.toUpperCase() === appSecretEnv.toUpperCase() || /^(?:FEISHU_ACTION_|CLAUDE_CODE_SESSION|CLAUDE_CODE_PARENT|CLAUDE_CODE_ENTRYPOINT|CLAUDE_CODE_SSE_PORT|CLAUDE_CODE_SDK|CLAUDE_AGENT_SDK|CLAUDE_CODE_TEAM|CLAUDE_CODE_AGENT|CLAUDE_CODE_TASK_LIST|CLAUDE_CODE_SUBAGENT|CLAUDECODE$)/i.test(key)) delete env[key];
    }
    Object.assign(env, { HOME: w.home, USERPROFILE: w.home });
    if (w.nativeConfig === 'inherit') {
        // An unset override has native meaning: global config remains at
        // ~/.claude.json rather than ~/.claude/.claude.json.
        if (process.env.CLAUDE_CONFIG_DIR) env.CLAUDE_CONFIG_DIR = w.configDir ?? process.env.CLAUDE_CONFIG_DIR;
        else delete env.CLAUDE_CONFIG_DIR;
    } else env.CLAUDE_CONFIG_DIR = w.configDir ?? join(w.home, '.claude');
    for (const name of ['CLAUDE_CODE_GIT_BASH_PATH', 'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY', 'NODE_EXTRA_CA_CERTS', 'SSL_CERT_FILE']) {
        if (process.env[name]) env[name] = process.env[name];
    }
    return env;
}

/** One persistent native process per active conversation; no container or model proxy. */
export function runtimeFactory(config: Config, secrets: string[], log: (event: string, detail: object) => void): WorkerFactory {
    return async (room: Room, session: Session, onEvent: (event: WorkerEvent) => void, prepared?: Workspace): Promise<Worker> => {
        const project = config.projects.find(p => p.id === room.projectId)!;
        const token = process.env[config.model.tokenEnv];
        if (!token) throw new Error(`missing model credential ${config.model.tokenEnv}`);
        const headers = modelHeaders(config);
        const w = prepared ?? await prepareWorkspace(config, room, session, project);
        const startedAt = performance.now(), bornAt = Date.now();
        const child = spawn(process.execPath, [fileURLToPath(new URL('./worker.js', import.meta.url))], {
            cwd: w.work, env: nativeEnv(w, config.appSecretEnv), stdio: ['pipe', 'pipe', 'pipe'], shell: false,
            windowsHide: true, detached: process.platform !== 'win32'
        });
        trackProcessTree(child, bornAt);
        let stopped = false, exited = false, reportedReady = false;
        let stopTask: Promise<void> | undefined;
        let unexpectedCleanup: Promise<void> | undefined;
        let unexpectedCleanupFailed = false;
        const cleanupUnexpected = (): Promise<void> => unexpectedCleanup ??= killTree(child).catch(error => {
            unexpectedCleanupFailed = true;
            log('worker_descendant_cleanup_failed', {
                room: hash(room.key).slice(0, 12), error: redact(String(error), secrets)
            });
        });
        const serial = new Serial();
        const write = async (value: object) => serial.run('stdin', async () => {
            if (stopped || exited) throw new Error('worker unavailable');
            await new Promise<void>((resolve, reject) => child.stdin.write(JSON.stringify(value) + '\n', error => error ? reject(error) : resolve()));
        });
        child.stdin.on('error', () => {});
        const streamKinds = new Set<string>();
        const parser = new JsonLines(value => {
            const event = value as unknown as WorkerEvent;
            if (event.type === 'ready' && !reportedReady) {
                reportedReady = true;
                log('worker_ready', { room: hash(room.key).slice(0, 12), startupMs: Math.round(performance.now() - startedAt), resumed: !!session.ccId });
            }
            if ((event.type === 'thinking' || event.type === 'text') && !streamKinds.has(event.type)) {
                streamKinds.add(event.type);
                log('worker_stream_started', { room: hash(room.key).slice(0, 12), kind: event.type, attributed: !!event.turnId });
            }
            if (event.type === 'result') streamKinds.clear();
            onEvent(event);
        });
        child.stdout.setEncoding('utf8');
        child.stdout.on('data', (chunk: string) => {
            try { parser.push(chunk); }
            catch {
                onEvent({ type: 'error', message: 'worker protocol violation' });
                void cleanupUnexpected();
            }
        });
        child.stderr.setEncoding('utf8');
        child.stderr.on('data', (chunk: string) => log('worker_stderr', {
            room: hash(room.key).slice(0, 12), text: redact(chunk, secrets.concat(token, ...Object.values(headers))).slice(0, 4000)
        }));
        child.on('error', error => onEvent({ type: 'error', message: redact(error.message, secrets) }));
        child.on('exit', () => {
            if (stopTask) return;
            // Do not report the room as reusable until descendant cleanup completes.
            void cleanupUnexpected();
        });
        child.on('close', code => {
            exited = true;
            // 'close' can also follow a spawn error without an 'exit' event.
            const cleanup = unexpectedCleanup ?? (stopTask ? Promise.resolve() : cleanupUnexpected());
            void cleanup.then(() => onEvent({
                type: 'exit', code, ...(unexpectedCleanupFailed ? { cleanupConfirmed: false } : {})
            })).catch(error => log('worker_exit_event_failed', {
                room: hash(room.key).slice(0, 12), error: redact(String(error), secrets)
            }));
        });
        try {
            await write({
                type: 'init', sessionId: session.id, ccId: session.ccId, workDir: w.work, homeDir: w.home, attachmentDir: w.inbound,
                nativeConfig: w.nativeConfig, configDir: w.configDir, roomsDir: dirname(w.root), claudeExecutable: config.runtime.claudeExecutable,
                approvalMs: config.limits.approvalMs,
                model: { url: config.model.baseUrl, token, model: config.model.model, smallModel: config.model.smallModel, headers },
                permissionMode: project.permissionMode, allowedTools: project.allowedTools,
                actions: project.actions.map(a => ({ id: a.id, description: a.description, parameters: a.parameters, readOnly: a.readOnly })),
                queueLimit: config.limits.inboxPerRoom
            });
        } catch (error) {
            await cleanupUnexpected();
            if (unexpectedCleanupFailed)
                onEvent({ type: 'exit', code: null, cleanupConfirmed: false });
            throw error;
        }
        return {
            send: delivery => write({ type: 'input', delivery }),
            respond: (id, value) => { void write({ type: 'response', id, value }).catch(() => {}); },
            stop: () => {
                if (!stopTask) stopTask = (async () => {
                    try { await Promise.race([write({ type: 'stop' }), delay(200)]); } catch { /* process already gone */ }
                    stopped = true;
                    try { await killTree(child); }
                    finally { await unexpectedCleanup; }
                    if (unexpectedCleanupFailed)
                        throw new Error('Native worker descendant termination unconfirmed');
                })();
                return stopTask;
            }
        };
    };
}
