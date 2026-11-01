import { spawn } from 'node:child_process';
import { randomUUID, createHash } from 'node:crypto';
import { mkdir, writeFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import type { ActionSpec, Args, Project, Room } from './types.ts';
import { Store } from './store.ts';
import { Approvals } from './approvals.ts';
import { hash, redact, readBoundedFile } from './util.ts';
import { killTree, trackProcessTree } from './process.ts';
export function actionArguments(spec: ActionSpec, input: Args, snapshots: Record<string, string> = {}): string[] {
    for (const name of Object.keys(input))
        if (!(name in spec.parameters))
            throw new Error(`unknown parameter: ${name}`);
    const values: Record<string, string> = {};
    for (const [name, p] of Object.entries(spec.parameters)) {
        const value = input[name];
        if (typeof value !== 'string' || value.length > p.maxLength || /[\0\r\n]/.test(value) || value.startsWith('-'))
            throw new Error(`invalid parameter: ${name}`);
        if (p.enum && !p.enum.includes(value))
            throw new Error(`invalid option: ${name}`);
        if (p.pattern && !new RegExp(`^(?:${p.pattern})$`, 'u').test(value))
            throw new Error(`parameter does not match policy: ${name}`);
        values[name] = value;
    }
    return spec.args.map(arg => arg.replace(/\{\{([^}]+)\}\}/g, (_, key: string) => snapshots[key] ?? values[key]!));
}
export function cleanEnv(extra: Record<string, string> = {}): NodeJS.ProcessEnv {
    const env: NodeJS.ProcessEnv = {};
    for (const name of ['PATH', 'SystemRoot', 'WINDIR', 'COMSPEC', 'PATHEXT', 'LANG', 'LC_ALL', 'TZ', 'TEMP', 'TMP'])
        if (process.env[name])
            env[name] = process.env[name];
    return { ...env, ...extra };
}
export async function runCommand(executable: string, args: string[], options: {
    cwd?: string;
    env?: NodeJS.ProcessEnv;
    timeoutMs?: number;
    maxBytes?: number;
    signal?: AbortSignal;
} = {}): Promise<{
    code: number;
    output: string;
}> {
    if (options.signal?.aborted)
        throw new Error('operation cancelled');
    return new Promise((resolve, reject) => {
        const bornAt = Date.now();
        const child = spawn(executable, args, { cwd: options.cwd, env: options.env ?? cleanEnv(), shell: false, windowsHide: true, detached: process.platform !== 'win32', stdio: ['ignore', 'pipe', 'pipe'] });
        trackProcessTree(child, bornAt);
        const chunks: Buffer[] = [];
        let bytes = 0, reason = '', finished = false;
        let fallback: NodeJS.Timeout | undefined;
        let termination: Promise<void> | undefined;
        const unconfirmed = () => new Error(`${reason}; process termination unconfirmed`);
        const stop = (why: string) => {
            if (reason || finished) return;
            reason = why;
            termination = killTree(child);
            // A child may close before the descendant cleanup promise settles.
            // Do not discard that outcome or let it become an unhandled rejection.
            void termination.catch(() => {});
            fallback = setTimeout(() => {
                if (finished) return;
                finished = true;
                child.stdout.destroy();
                child.stderr.destroy();
                cleanup();
                reject(unconfirmed());
            }, 5000);
            fallback.unref();
        };
        const collect = (chunk: Buffer) => { bytes += chunk.length; if (bytes > (options.maxBytes ?? 1024 * 1024)) {
            stop('output size limit');
            return;
        } chunks.push(chunk); };
        child.stdout.on('data', collect);
        child.stderr.on('data', collect);
        const timer = setTimeout(() => stop('operation timed out'), options.timeoutMs ?? 60000);
        timer.unref();
        const abort = () => stop('operation cancelled');
        options.signal?.addEventListener('abort', abort, { once: true });
        const cleanup = () => { clearTimeout(timer); clearTimeout(fallback); options.signal?.removeEventListener('abort', abort); };
        child.once('error', e => { if (finished)
            return; finished = true; cleanup(); reject(e); });
        child.once('close', code => {
            if (finished) return;
            if (reason && termination) {
                // The root's closed pipes do not prove descendants were cleaned.
                // Resolve the command only after the actual termination result.
                void termination.then(
                    () => { if (finished) return; finished = true; cleanup(); reject(new Error(reason)); },
                    () => { if (finished) return; finished = true; cleanup(); reject(unconfirmed()); }
                );
                return;
            }
            finished = true;
            cleanup();
            resolve({ code: code ?? 1, output: Buffer.concat(chunks).toString('utf8') });
        });
    });
}
export class ActionBroker {
    readonly store: Store;
    readonly approvals: Approvals;
    readonly secrets: string[];
    readonly staging?: string;
    constructor(store: Store, approvals: Approvals, secrets: string[], staging?: string) { this.store = store; this.approvals = approvals; this.secrets = secrets; this.staging = staging; }
    async execute(room: Room, session: string, project: Project, id: string, input: Args, signal: AbortSignal, workspace?: string, resolveFile: (path: string) => string = path => path): Promise<Args> {
        const spec = project.actions.find(a => a.id === id);
        if (!spec)
            throw new Error('action not allowed in this project');
        actionArguments(spec, input);
        const run = randomUUID(), snapshots: Record<string, string> = {}, fileDigests: Record<string, string> = {};
        const staging = this.staging ? join(this.staging, run) : undefined;
        try {
            for (const [name, parameter] of Object.entries(spec.parameters))
                if (parameter.workspaceFile) {
                    if (!workspace || !staging)
                        throw new Error('workspace file snapshots are unavailable');
                    const bytes = await readBoundedFile(workspace, resolveFile(String(input[name])), 10 * 1024 * 1024);
                    await mkdir(staging, { recursive: true, mode: 0o700 });
                    const path = join(staging, name);
                    await writeFile(path, bytes, { mode: 0o600 });
                    snapshots[name] = path;
                    fileDigests[name] = createHash('sha256').update(bytes).digest('hex');
                }
            const args = actionArguments(spec, input, snapshots), digest = hash(JSON.stringify({ id, input, fileDigests }));
            if (!spec.readOnly) {
                const decision = await this.approvals.request(room, session, id, input, `项目：${project.id}\n操作：${spec.description}\n参数：\`${redact(JSON.stringify(input), this.secrets).slice(0, 4000)}\`\n文件快照 SHA256：\`${JSON.stringify(fileDigests)}\`\n摘要：\`${digest.slice(0, 16)}\``, signal);
                if (!decision.approved)
                    return { ok: false, error: decision.reason ?? 'not approved' };
            }
            if (signal.aborted)
                return { ok: false, error: 'cancelled' };
            const env: NodeJS.ProcessEnv = cleanEnv(spec.vars);
            for (const [key, envName] of Object.entries(spec.env)) {
                const value = process.env[envName];
                if (value === undefined)
                    throw new Error(`missing configured secret: ${envName}`);
                env[key] = value;
            }
            this.store.db.prepare('INSERT INTO action_runs VALUES(?,?,?,?,?,?,?)').run(run, room.key, session, id, digest, 'running', Date.now());
            this.store.audit(room.key, 'action_started', { run, action: id, digest, readOnly: spec.readOnly });
            try {
                const result = await runCommand(spec.executable, args, { cwd: spec.cwd, env, timeoutMs: spec.timeoutMs, signal });
                this.store.db.prepare('UPDATE action_runs SET state=? WHERE id=?').run(result.code === 0 ? 'done' : 'failed', run);
                this.store.audit(room.key, 'action_finished', { run, action: id, code: result.code });
                return { ok: result.code === 0, code: result.code, output: redact(result.output, [...this.secrets, ...Object.values(spec.env).map(name => process.env[name]).filter((v): v is string => !!v)]) };
            }
            catch (e) {
                this.store.db.prepare("UPDATE action_runs SET state='uncertain' WHERE id=?").run(run);
                if (e instanceof Error && e.message.includes('process termination unconfirmed'))
                    this.store.audit(room.key, 'action_termination_unconfirmed', { run, action: id });
                throw e;
            }
        }
        finally {
            if (staging)
                await rm(staging, { recursive: true, force: true });
        }
    }
}
