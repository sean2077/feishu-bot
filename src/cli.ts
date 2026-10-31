#!/usr/bin/env node
import { readFile, writeFile, mkdir, stat } from 'node:fs/promises';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { loadConfig, modelHeaders, secretsFor } from './config.ts';
import { lockDataDir, redact, errorText } from './util.ts';
import { runCommand } from './actions.ts';
import { Store } from './store.ts';
const args = process.argv.slice(2);
const command = args[0] ?? 'help';
const option = (name: string, fallback: string) => { const i = args.indexOf(name); return i < 0 ? fallback : (args[i + 1] ?? fallback); };
const configPath = resolve(option('--config', 'config.json'));
let secrets: string[] = [];
async function main(): Promise<void> {
    if (command === 'help' || command === '--help') {
        console.log('feishu-bot init [--config path]\nfeishu-bot doctor [--config path] [--probe]\nfeishu-bot run [--config path]\n\nLinux / Windows: run one native service. Claude sessions start on demand and stay warm between messages.');
        return;
    }
    if (command === 'init') {
        const example = await readFile(fileURLToPath(new URL('../examples/config.example.json', import.meta.url)), 'utf8');
        await mkdir(dirname(configPath), { recursive: true });
        try { await writeFile(configPath, example, { flag: 'wx', mode: 0o600 }); }
        catch (error) {
            if ((error as NodeJS.ErrnoException).code === 'EEXIST') throw new Error(`${configPath} already exists; it was not overwritten.`);
            throw error;
        }
        console.log(`Created ${configPath}. Fill in app/chat/member IDs and company model settings, then run npm run doctor and npm start.`);
        return;
    }
    const config = await loadConfig(configPath).catch(error => {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') throw new Error(`Missing ${configPath}. Run npm run init first.`);
        throw error;
    });
    secrets = secretsFor(config);
    const log = (event: string, detail: object = {}) => process.stderr.write(redact(JSON.stringify({ at: new Date().toISOString(), event, ...detail }), secrets) + '\n');
    if (command === 'doctor') {
        const checks: { name: string; ok: boolean; detail?: string }[] = [];
        checks.push({ name: 'Node >=22.16', ok: Number(process.versions.node.split('.')[0]) > 22 || (Number(process.versions.node.split('.')[0]) === 22 && Number(process.versions.node.split('.')[1]) >= 16), detail: process.versions.node });
        for (const name of [config.appSecretEnv, config.model.tokenEnv]) checks.push({ name: `credential ${name}`, ok: !!process.env[name] });
        try { modelHeaders(config); checks.push({ name: 'company gateway headers', ok: true }); }
        catch (error) { checks.push({ name: 'company gateway headers', ok: false, detail: errorText(error) }); }
        try { const git = await runCommand('git', ['--version']); checks.push({ name: 'Git', ok: git.code === 0, detail: git.output.trim() }); }
        catch { checks.push({ name: 'Git', ok: false, detail: 'Install Git; on Windows install Git for Windows.' }); }
        if (process.platform === 'win32' && process.env.CLAUDE_CODE_GIT_BASH_PATH) {
            try { checks.push({ name: 'configured Git Bash', ok: (await stat(process.env.CLAUDE_CODE_GIT_BASH_PATH)).isFile() }); }
            catch { checks.push({ name: 'configured Git Bash', ok: false }); }
        }
        await mkdir(config.dataDir, { recursive: true, mode: 0o700 });
        try { const probe = new Store(join(config.dataDir, 'doctor.sqlite')); probe.close(); checks.push({ name: 'SQLite', ok: true }); }
        catch { checks.push({ name: 'SQLite', ok: false }); }
        for (const project of config.projects) {
            for (const [name, path] of [['seedDir', project.seedDir], ['trustedConfigDir', project.trustedConfigDir]] as const) if (path) {
                try { checks.push({ name: `${project.id} ${name}`, ok: (await stat(path)).isDirectory() }); }
                catch { checks.push({ name: `${project.id} ${name}`, ok: false }); }
            }
            for (const action of project.actions) {
                try { checks.push({ name: `action ${project.id}/${action.id}`, ok: (await stat(action.executable)).isFile() }); }
                catch { checks.push({ name: `action ${project.id}/${action.id}`, ok: false }); }
                for (const env of Object.values(action.env)) checks.push({ name: `action credential ${env}`, ok: !!process.env[env] });
            }
        }
        if (args.includes('--probe')) {
            try {
                const result = await fetch(config.model.baseUrl + '/v1/messages', {
                    method: 'POST', headers: { 'content-type': 'application/json', 'anthropic-version': '2023-06-01', authorization: `Bearer ${process.env[config.model.tokenEnv] ?? ''}`, ...modelHeaders(config) },
                    body: JSON.stringify({ model: config.model.model, max_tokens: 16, messages: [{ role: 'user', content: 'Reply OK.' }] }), signal: AbortSignal.timeout(30000), redirect: 'manual'
                });
                checks.push({ name: 'company gateway basic probe (not full harness acceptance)', ok: result.ok, detail: `HTTP ${result.status}` });
                await result.body?.cancel();
            } catch { checks.push({ name: 'company gateway basic probe', ok: false }); }
        }
        console.log(JSON.stringify({ platform: process.platform, runtime: 'native', checks }, null, 2));
        if (checks.some(c => !c.ok)) process.exitCode = 1;
        return;
    }
    if (command !== 'run') throw new Error('Unknown command; use --help');
    if (!process.env[config.model.tokenEnv]) throw new Error(`Missing company model credential ${config.model.tokenEnv}`);
    modelHeaders(config);
    const unlock = await lockDataDir(config.dataDir);
    let store: Store | undefined;
    let gateway: import('./gateway.ts').Gateway | undefined;
    let channel: ReturnType<typeof import('./feishu.ts').feishuTransport>['channel'] | undefined;
    let closed = false;
    const shutdown = async () => {
        if (closed) return;
        closed = true; log('shutdown');
        try { await channel?.disconnect().catch(() => {}); await gateway?.close(); }
        finally { store?.close(); await unlock(); }
    };
    try {
        store = new Store(join(config.dataDir, 'gateway.sqlite'));
        const { feishuTransport, normalize } = await import('./feishu.ts');
        const { runtimeFactory } = await import('./runtime.ts');
        const { Gateway } = await import('./gateway.ts');
        const connected = feishuTransport(config, log);
        channel = connected.channel;
        gateway = new Gateway(config, store, connected.transport, runtimeFactory(config, secrets, log), secrets);
        process.once('SIGINT', () => void shutdown());
        process.once('SIGTERM', () => void shutdown());
        channel.on('message', async message => { try { await gateway!.message(normalize(message)); } catch (e) { log('inbound_failed', { error: errorText(e) }); } });
        channel.on('cardAction', async event => ({ toast: { type: 'info', content: await gateway!.approvals.click(event.chatId, event.operator.openId, event.action.value, event.messageId) } }));
        channel.on('error', error => log('feishu_error', { code: error.code }));
        channel.on('reconnecting', () => log('feishu_reconnecting'));
        channel.on('reconnected', () => log('feishu_reconnected'));
        await channel.connect();
        await gateway.recover();
        log('ready', { mode: 'native', groups: config.groups.length, dms: config.dms.length });
    } catch (error) { await shutdown(); throw error; }
}
main().catch(error => { console.error(redact(errorText(error), secrets)); process.exitCode = 1; });
