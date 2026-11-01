import { mkdir, mkdtemp, rm, stat } from 'node:fs/promises';
import { basename, join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Config } from './types.ts';
import { modelHeaders, secretsFor } from './config.ts';
import { runCommand } from './actions.ts';
import { errorText, redact } from './util.ts';

export interface Diagnostic { name: string; ok: boolean; detail?: string; skipped?: boolean; }
export interface DoctorReport { platform: string; runtime: 'native'; ready: boolean; scope: string; checks: Diagnostic[]; }

function placeholder(value: string): boolean {
    return /^(?:your[-_ ]|replace[-_ ]with[-_ ]|<[^>]+>$)/i.test(value.trim()) ||
        /^(?:(?:tenant|cli|oc|ou)_example)(?:_|$)/i.test(value.trim());
}
function exampleEndpoint(baseUrl: string): boolean {
    const host = new URL(baseUrl).hostname.toLowerCase().replace(/\.$/, '');
    return /(?:^|\.)(?:example|invalid)$/.test(host) || /(?:^|\.)example\.(?:com|net|org)$/.test(host);
}

/** Deployment checks are separate from parsing: valid templates remain editable/testable. */
export function deploymentChecks(config: Config): Diagnostic[] {
    const unfinished: string[] = [];
    const value = (name: string, content: string) => { if (placeholder(content)) unfinished.push(name); };
    value('tenantId', config.tenantId); value('appId', config.appId);
    value('model.model', config.model.model); value('model.smallModel', config.model.smallModel);
    if (exampleEndpoint(config.model.baseUrl)) unfinished.push('model.baseUrl');
    config.projects.forEach((project, i) => project.maintainers.forEach((id, j) => value(`projects[${i}].maintainers[${j}]`, id)));
    config.groups.forEach((group, i) => {
        value(`groups[${i}].chatId`, group.chatId);
        group.members?.forEach((id, j) => value(`groups[${i}].members[${j}]`, id));
    });
    config.dms.forEach((dm, i) => value(`dms[${i}].userId`, dm.userId));
    const checks: Diagnostic[] = [
        { name: 'configuration values', ok: !unfinished.length, detail: unfinished.length ? `Replace template values in: ${unfinished.join(', ')}.` : 'No known template placeholders.' },
        { name: 'chat bindings', ok: config.groups.length + config.dms.length > 0,
            detail: config.groups.length + config.dms.length ? `${config.groups.length} configured group(s), ${config.dms.length} configured DM user(s). IDs and platform access still require live verification.` : 'Configure at least one authorized group or DM user; empty lists accept no messages.' }
    ];
    const names = new Set([config.appSecretEnv, config.model.tokenEnv,
        ...config.projects.flatMap(p => p.actions.flatMap(a => Object.values(a.env)))]);
    for (const name of names) {
        const secret = process.env[name];
        const ok = !!secret?.trim() && !placeholder(secret);
        checks.push({ name: `credential ${name}`, ok, detail: ok ? 'Present; value is not displayed.' : `Set ${name} in the service environment to a nonempty credential, not an example value.` });
    }
    try {
        const headers = modelHeaders(config);
        if (Object.values(headers).some(value => !value.trim() || placeholder(value)))
            throw new Error('Replace empty or template model header values before starting.');
        checks.push({ name: 'model endpoint headers', ok: true });
    }
    catch (error) { checks.push({ name: 'model endpoint headers', ok: false, detail: redact(errorText(error), secretsFor(config)) }); }
    return checks;
}

export async function runDoctor(config: Config, probe = false): Promise<DoctorReport> {
    const secrets = secretsFor(config);
    const checks = deploymentChecks(config);
    const [major = 0, minor = 0] = process.versions.node.split('.').map(Number);
    checks.unshift({ name: 'Node >=22.16', ok: major > 22 || (major === 22 && minor >= 16), detail: process.versions.node });
    const check = async (name: string, fn: () => Promise<string | void>) => {
        try { const detail = await fn(); checks.push({ name, ok: true, ...(detail ? { detail } : {}) }); }
        catch (error) { checks.push({ name, ok: false, detail: redact(errorText(error), secrets) }); }
    };
    await check('Git', async () => {
        const result = await runCommand('git', ['--version'], { timeoutMs: 5000, maxBytes: 4096 });
        if (result.code !== 0) throw new Error('git --version failed. Install Git and check PATH (Git for Windows on Windows).');
        return result.output.trim();
    });
    if (process.platform === 'win32' && process.env.CLAUDE_CODE_GIT_BASH_PATH) {
        await check('configured Git Bash', async () => {
            const executable = process.env.CLAUDE_CODE_GIT_BASH_PATH!;
            const invalid = 'CLAUDE_CODE_GIT_BASH_PATH must point to a working Git for Windows bash.exe.';
            if (basename(executable).toLowerCase() !== 'bash.exe' ||
                !(await stat(executable).then(s => s.isFile()).catch(() => false)))
                throw new Error(invalid);
            // A readable file (even node.exe renamed bash.exe) is not proof that
            // Claude can start its configured shell. Do not echo the private path.
            let result;
            try { result = await runCommand(executable, ['--version'], { timeoutMs: 5000, maxBytes: 4096 }); }
            catch { throw new Error(invalid); }
            if (result.code !== 0 || !/GNU bash, version/i.test(result.output)) throw new Error(invalid);
            return 'Configured Git Bash launches and reports its version.';
        });
    }
    await check('data directory / SQLite', async () => {
        // Never open or migrate the live database, or reuse another diagnostic's filename.
        let dir: string | undefined, db: DatabaseSync | undefined;
        try {
            await mkdir(config.dataDir, { recursive: true, mode: 0o700 });
            dir = await mkdtemp(join(config.dataDir, '.doctor-'));
            db = new DatabaseSync(join(dir, 'probe.sqlite'));
            db.exec('PRAGMA journal_mode=WAL; CREATE TABLE probe(value TEXT); INSERT INTO probe VALUES(\'ok\');');
            if (db.prepare('SELECT value FROM probe').get()?.value !== 'ok') throw new Error('SQLite read/write check failed.');
        } finally {
            try { db?.close(); }
            finally { if (dir) await rm(dir, { recursive: true, force: true }); }
        }
        return 'Writable; temporary SQLite probe was removed. Existing state was not opened.';
    });
    for (const project of config.projects) {
        for (const [name, path] of [['seedDir', project.seedDir], ['trustedConfigDir', project.trustedConfigDir]] as const) if (path) {
            await check(`${project.id} ${name}`, async () => {
                if (!(await stat(path)).isDirectory()) throw new Error(`${name} must be an existing directory.`);
            });
        }
        for (const action of project.actions) {
            await check(`action ${project.id}/${action.id}`, async () => {
                if (!(await stat(action.executable)).isFile()) throw new Error('Action executable must be an existing file.');
                if (action.cwd && !(await stat(action.cwd)).isDirectory()) throw new Error('Action cwd must be an existing directory.');
            });
        }
    }
    let probeSent = false;
    if (probe) {
        if (deploymentChecks(config).some(c => !c.ok)) {
            checks.push({ name: 'model endpoint basic probe', ok: false, skipped: true,
                detail: 'Not sent: correct the configuration and credential checks first. No model request was made.' });
        } else await check('model endpoint basic probe', async () => {
            probeSent = true;
            const response = await fetch(config.model.baseUrl + '/v1/messages', {
                method: 'POST', redirect: 'manual', signal: AbortSignal.timeout(30000),
                headers: { 'content-type': 'application/json', 'anthropic-version': '2023-06-01',
                    authorization: `Bearer ${process.env[config.model.tokenEnv]}`, ...modelHeaders(config) },
                body: JSON.stringify({ model: config.model.model, max_tokens: 16, messages: [{ role: 'user', content: 'Reply OK.' }] })
            });
            if (!response.ok) {
                await response.body?.cancel();
                const hint = response.status === 401 || response.status === 403 ? 'Check credentials and model access.'
                    : response.status === 404 ? 'Check baseUrl and the model ID.'
                    : response.status === 429 ? 'Check upstream rate limits and quota.' : 'Inspect the model service status.';
                throw new Error(`HTTP ${response.status}. ${hint} Response body is not included.`);
            }
            const reader = response.body?.getReader();
            const chunks: Uint8Array[] = [];
            let bytes = 0;
            try {
                if (reader) for (;;) {
                    const { value, done } = await reader.read();
                    if (done) break;
                    bytes += value.byteLength;
                    if (bytes > 65536) throw new Error('Probe response exceeded 64 KiB. Check the Messages endpoint.');
                    chunks.push(value);
                }
            } finally { await reader?.cancel().catch(() => {}); }
            let data: Record<string, unknown>;
            try { data = JSON.parse(Buffer.concat(chunks).toString('utf8')); }
            catch { throw new Error(`HTTP ${response.status}, but not a JSON Messages response. Check baseUrl (not a login or UI page).`); }
            if (!data || data.type !== 'message' || data.role !== 'assistant' || !Array.isArray(data.content) ||
                !data.content.some(b => b?.type === 'text' && typeof b.text === 'string' && b.text.trim()))
                throw new Error('The endpoint did not return an assistant Messages response. HTTP success alone is not sufficient.');
            return 'Basic Messages response verified. Streaming, tools and live Feishu behavior were not tested.';
        });
    }
    return { platform: process.platform, runtime: 'native', ready: checks.every(c => c.ok),
        scope: probeSent ? 'Local checks and one basic model request; not full deployment acceptance.' : 'Local checks only; no Feishu or model requests were made.', checks };
}
