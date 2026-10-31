import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import type { Config, Room, Session, Worker, WorkerEvent, WorkerFactory, Workspace } from './types.ts';
import { cleanEnv } from './actions.ts';
import { modelHeaders } from './config.ts';
import { JsonLines, killTree } from './process.ts';
import { prepareWorkspace } from './workspace.ts';
import { hash, redact, Serial } from './util.ts';

/** Only the required native toolchain/network settings cross into a session. */
export function nativeEnv(w: Workspace): NodeJS.ProcessEnv {
    const env = cleanEnv({ HOME: w.home, USERPROFILE: w.home, CLAUDE_CONFIG_DIR: join(w.home, '.claude'), TZ: 'Asia/Singapore' });
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
        const startedAt = performance.now();
        const child = spawn(process.execPath, [fileURLToPath(new URL('./worker.js', import.meta.url))], {
            cwd: w.work, env: nativeEnv(w), stdio: ['pipe', 'pipe', 'pipe'], shell: false,
            windowsHide: true, detached: process.platform !== 'win32'
        });
        let stopped = false, exited = false, reportedReady = false;
        const serial = new Serial();
        const write = async (value: object) => serial.run('stdin', async () => {
            if (stopped || exited) throw new Error('worker unavailable');
            await new Promise<void>((resolve, reject) => child.stdin.write(JSON.stringify(value) + '\n', error => error ? reject(error) : resolve()));
        });
        child.stdin.on('error', () => {});
        const parser = new JsonLines(value => {
            const event = value as unknown as WorkerEvent;
            if (event.type === 'ready' && !reportedReady) {
                reportedReady = true;
                log('worker_ready', { room: hash(room.key).slice(0, 12), startupMs: Math.round(performance.now() - startedAt), resumed: !!session.ccId });
            }
            onEvent(event);
        });
        child.stdout.setEncoding('utf8');
        child.stdout.on('data', (chunk: string) => {
            try { parser.push(chunk); }
            catch { onEvent({ type: 'error', message: 'worker protocol violation' }); void killTree(child); }
        });
        child.stderr.setEncoding('utf8');
        child.stderr.on('data', (chunk: string) => log('worker_stderr', {
            room: hash(room.key).slice(0, 12), text: redact(chunk, secrets.concat(token, ...Object.values(headers))).slice(0, 4000)
        }));
        child.on('error', error => onEvent({ type: 'error', message: redact(error.message, secrets) }));
        child.on('close', code => { exited = true; onEvent({ type: 'exit', code }); });
        try {
            await write({
                type: 'init', sessionId: session.id, ccId: session.ccId, workDir: w.work, homeDir: w.home, attachmentDir: w.inbound,
                model: { url: config.model.baseUrl, token, model: config.model.model, smallModel: config.model.smallModel, headers },
                permissionMode: project.permissionMode, allowedTools: project.allowedTools,
                actions: project.actions.map(a => ({ id: a.id, description: a.description, parameters: a.parameters, readOnly: a.readOnly })),
                queueLimit: config.limits.inboxPerRoom
            });
        } catch (error) { await killTree(child); throw error; }
        return {
            send: delivery => write({ type: 'input', delivery }),
            respond: (id, value) => { void write({ type: 'response', id, value }).catch(() => {}); },
            stop: async () => {
                if (stopped) return;
                try { await Promise.race([write({ type: 'stop' }), delay(200)]); } catch { /* process already gone */ }
                stopped = true;
                await killTree(child);
            }
        };
    };
}
