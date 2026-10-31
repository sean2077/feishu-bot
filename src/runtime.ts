import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import type { Config, Delivery, Room, Session, Worker, WorkerEvent, WorkerFactory } from './types.ts';
import { cleanEnv, runCommand } from './actions.ts';
import { JsonLines, killTree } from './process.ts';
import { prepareWorkspace, workerPaths } from './workspace.ts';
import { hash, redact, Serial } from './util.ts';
import { ModelBudget, startModelProxy } from './model-proxy.ts';
export function dockerEnv(): NodeJS.ProcessEnv { const env = cleanEnv(); for (const name of ['HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH', 'XDG_RUNTIME_DIR'])
    if (process.env[name])
        env[name] = process.env[name]; return env; }
export function instanceLabel(config: Config): string { return hash(config.appId + '\n' + config.dataDir).slice(0, 32); }
export async function cleanupOwnedContainers(config: Config): Promise<void> {
    if (config.runtime.mode !== 'docker')
        return;
    const result = await runCommand('docker', ['ps', '-aq', '--filter', `label=io.feishu-bot.instance=${instanceLabel(config)}`], { timeoutMs: 10000, env: dockerEnv() });
    if (result.code !== 0)
        throw new Error('Cannot inspect owned Docker workers');
    const ids = result.output.trim().split(/\s+/).filter(Boolean);
    if (ids.some(id => !(/^[a-f0-9]{12,64}$/).test(id)))
        throw new Error('Unexpected Docker container identifier');
    if (ids.length) {
        const removed = await runCommand('docker', ['rm', '-f', ...ids], { timeoutMs: 20000, env: dockerEnv() });
        if (removed.code !== 0)
            throw new Error('Cannot stop orphaned workers');
    }
}
export function dockerArgs(config: Config, room: Room, w: {
    work: string;
    home: string;
    socketDir: string;
    trusted: string;
    inbound: string;
}, name: string): string[] {
    const uid = process.getuid?.() ?? 1000, gid = process.getgid?.() ?? 1000;
    if (uid === 0)
        throw new Error('Run the gateway as an unprivileged service account, not root');
    for (const path of [w.work, w.home, w.socketDir, w.trusted, w.inbound])
        if (path.includes(','))
            throw new Error('Docker mount paths cannot contain commas');
    return ['run', '--rm', '-i', '--init', '--name', name, '--network', 'none', '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
        '--label', `io.feishu-bot.instance=${instanceLabel(config)}`, '--pids-limit', '256', '--memory', config.runtime.memory, '--cpus', String(config.runtime.cpus), '--user', `${uid}:${gid}`,
        '--tmpfs', '/tmp:rw,nosuid,nodev,size=256m', '--mount', `type=bind,source=${w.work},target=/workspace`,
        '--mount', `type=bind,source=${w.home},target=/home/worker`,
        '--mount', `type=bind,source=${w.socketDir},target=/run/model,readonly`,
        '--mount', `type=bind,source=${w.trusted},target=/workspace/.claude,readonly`,
        '--mount', `type=bind,source=${w.inbound},target=/attachments,readonly`,
        '--workdir', '/workspace', '--env', 'HOME=/home/worker', '--env', 'CLAUDE_CONFIG_DIR=/home/worker/.claude', config.runtime.image];
}
export function runtimeFactory(config: Config, secrets: string[], log: (event: string, detail: object) => void): WorkerFactory {
    const budget = new ModelBudget(config.model.maxConcurrent);
    return async (room: Room, session: Session, onEvent: (event: WorkerEvent) => void): Promise<Worker> => {
        const project = config.projects.find(p => p.id === room.projectId)!;
        const w = await prepareWorkspace(config, room, session, project), paths = workerPaths(config, w);
        const socket = config.runtime.mode === 'docker' ? join(w.socketDir, 'model.sock') : undefined;
        if (socket && Buffer.byteLength(socket) > 100)
            throw new Error('dataDir path is too long for the model UNIX socket; use a shorter Linux state path');
        const name = `feishu-bot-${hash(room.key).slice(0, 16)}-${session.id.slice(0, 8)}`;
        const docker = config.runtime.mode === 'docker' ? dockerArgs(config, room, w, name) : undefined;
        const proxy = await startModelProxy(config, budget, socket);
        const env = cleanEnv({ HOME: w.home, USERPROFILE: w.home, CLAUDE_CONFIG_DIR: join(w.home, '.claude'), TZ: 'Asia/Singapore' });
        if (process.env.CLAUDE_CODE_GIT_BASH_PATH)
            env.CLAUDE_CODE_GIT_BASH_PATH = process.env.CLAUDE_CODE_GIT_BASH_PATH;
        const child = config.runtime.mode === 'docker'
            ? spawn('docker', docker!, { env: dockerEnv(), stdio: ['pipe', 'pipe', 'pipe'], shell: false, windowsHide: true, detached: process.platform !== 'win32' })
            : spawn(process.execPath, [fileURLToPath(new URL('./worker.js', import.meta.url))], { cwd: w.work, env, stdio: ['pipe', 'pipe', 'pipe'], shell: false, windowsHide: true, detached: process.platform !== 'win32' });
        let stopped = false, exited = false;
        const serial = new Serial();
        const write = async (value: object) => serial.run('stdin', async () => { if (stopped || exited)
            throw new Error('worker unavailable'); await new Promise<void>((resolve, reject) => child.stdin.write(JSON.stringify(value) + '\n', error => error ? reject(error) : resolve())); });
        child.stdin.on('error', () => { });
        const parser = new JsonLines(value => onEvent(value as unknown as WorkerEvent));
        child.stdout.setEncoding('utf8');
        child.stdout.on('data', (chunk: string) => { try {
            parser.push(chunk);
        }
        catch {
            onEvent({ type: 'error', message: 'worker protocol violation' });
            void killTree(child);
        } });
        child.stderr.setEncoding('utf8');
        child.stderr.on('data', (chunk: string) => log('worker_stderr', { room: hash(room.key).slice(0, 12), text: redact(chunk, secrets.concat(proxy.token)).slice(0, 4000) }));
        child.on('error', error => onEvent({ type: 'error', message: redact(error.message, secrets) }));
        child.on('close', code => { exited = true; void proxy.close().catch(() => { }); onEvent({ type: 'exit', code }); });
        try {
            await write({ type: 'init', sessionId: session.id, ccId: session.ccId, workDir: paths.work, homeDir: paths.home,
                model: { url: proxy.url, socket: proxy.socket ? '/run/model/model.sock' : undefined, token: proxy.token, model: config.model.model, smallModel: config.model.smallModel },
                autoApproveWorkspace: project.autoApproveWorkspace, actions: project.actions.map(a => ({ id: a.id, description: a.description, parameters: a.parameters, readOnly: a.readOnly })),
                queueLimit: config.limits.inboxPerRoom });
        }
        catch (error) {
            await killTree(child);
            await proxy.close();
            throw error;
        }
        return {
            send: async (delivery: Delivery) => { const mapped = { ...delivery, attachments: delivery.attachments.map(a => ({ ...a, path: config.runtime.mode === 'docker' ? '/attachments/' + a.path.split(/[\\/]/).pop() : a.path })) }; await write({ type: 'input', delivery: mapped }); },
            respond: (id, value) => { void write({ type: 'response', id, value }).catch(() => { }); },
            stop: async () => {
                if (stopped)
                    return;
                try {
                    await Promise.race([write({ type: 'stop' }), delay(200)]);
                }
                catch { /* process gone */ }
                stopped = true;
                // Do not let a CLI approval or detached task keep the execution boundary alive.
                if (config.runtime.mode === 'docker')
                    await runCommand('docker', ['stop', '--time', '2', name], { timeoutMs: 5000, env: dockerEnv() }).catch(() => { });
                await killTree(child);
                await proxy.close().catch(() => { });
            }
        };
    };
}
