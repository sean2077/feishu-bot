#!/usr/bin/env node
import { readFile, writeFile, mkdir, stat } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { loadConfig, secretsFor } from './config.ts';
import { lockDataDir, redact, errorText } from './util.ts';
import { runCommand } from './actions.ts';
import { Store } from './store.ts';
const args = process.argv.slice(2);
const command = args[0] ?? 'help';
const option = (name: string, fallback: string) => { const i = args.indexOf(name); return i < 0 ? fallback : (args[i + 1] ?? fallback); };
const configPath = resolve(option('--config', 'config.json'));
async function main(): Promise<void> {
    if (command === 'help' || command === '--help') {
        console.log('feishu-bot init [--config path]\nfeishu-bot doctor [--config path] [--probe]\nfeishu-bot run [--config path]\n\nLinux production: Docker worker image. Windows development: local runtime with explicit approvals.');
        return;
    }
    if (command === 'init') {
        const example = await readFile(fileURLToPath(new URL('../examples/config.example.json', import.meta.url)), 'utf8');
        await writeFile(configPath, example, { flag: 'wx', mode: 0o600 });
        console.log(`Created ${configPath}. Edit IDs and set secrets through environment variables.`);
        return;
    }
    const config = await loadConfig(configPath), secrets = secretsFor(config);
    const log = (event: string, detail: object = {}) => process.stderr.write(redact(JSON.stringify({ at: new Date().toISOString(), event, ...detail }), secrets) + '\n');
    if (command === 'doctor') {
        const checks: {
            name: string;
            ok: boolean;
            detail?: string;
        }[] = [];
        checks.push({ name: 'Node >=22.16', ok: Number(process.versions.node.split('.')[0]) > 22 || (Number(process.versions.node.split('.')[0]) === 22 && Number(process.versions.node.split('.')[1]) >= 16), detail: process.versions.node });
        for (const name of [config.appSecretEnv, config.model.tokenEnv])
            checks.push({ name: `credential ${name}`, ok: !!process.env[name] });
        await mkdir(config.dataDir, { recursive: true, mode: 0o700 });
        try {
            const probe = new Store(join(config.dataDir, 'doctor.sqlite'));
            probe.close();
            checks.push({ name: 'SQLite', ok: true });
        }
        catch {
            checks.push({ name: 'SQLite', ok: false });
        }
        if (config.runtime.mode === 'docker') {
            const { dockerEnv } = await import('./runtime.ts');
            checks.push({ name: 'Linux host', ok: process.platform === 'linux' });
            checks.push({ name: 'unprivileged gateway account', ok: process.getuid?.() !== 0 });
            for (const [name, params] of [['Docker daemon', ['info', '--format', '{{.ServerVersion}}']], ['Worker image', ['image', 'inspect', config.runtime.image, '--format', '{{.Id}}']]] as const) {
                try {
                    const r = await runCommand('docker', [...params], { timeoutMs: 10000, env: dockerEnv() });
                    checks.push({ name, ok: r.code === 0, detail: r.output.trim().slice(0, 150) });
                }
                catch {
                    checks.push({ name, ok: false });
                }
            }
        }
        else
            checks.push({ name: 'local mode: trusted test use only', ok: true, detail: 'Not an OS security boundary. Do not share production credentials.' });
        for (const project of config.projects)
            for (const action of project.actions) {
                try {
                    checks.push({ name: `action ${project.id}/${action.id}`, ok: (await stat(action.executable)).isFile() });
                }
                catch {
                    checks.push({ name: `action ${project.id}/${action.id}`, ok: false });
                }
                for (const env of Object.values(action.env))
                    checks.push({ name: `action credential ${env}`, ok: !!process.env[env] });
            }
        if (args.includes('--probe')) {
            // Explicitly opt in: this costs model tokens, but uses no project content.
            try {
                const result = await fetch(config.model.baseUrl.replace(/\/$/, '') + (config.model.baseUrl.endsWith('/v1') ? '/messages' : '/v1/messages'), {
                    method: 'POST', headers: { 'content-type': 'application/json', 'anthropic-version': '2023-06-01', authorization: `Bearer ${process.env[config.model.tokenEnv] ?? ''}`, ...Object.fromEntries(Object.entries(config.model.extraHeaders).map(([k, v]) => [k, v.startsWith('env:') ? (process.env[v.slice(4)] ?? '') : v])) },
                    body: JSON.stringify({ model: config.model.model, max_tokens: 16, messages: [{ role: 'user', content: 'Reply OK.' }] }), signal: AbortSignal.timeout(30000), redirect: 'manual'
                });
                checks.push({ name: 'company gateway basic probe (not full harness acceptance)', ok: result.ok, detail: `HTTP ${result.status}` });
                await result.body?.cancel();
            }
            catch {
                checks.push({ name: 'company gateway basic probe', ok: false });
            }
        }
        console.log(JSON.stringify({ platform: process.platform, checks }, null, 2));
        if (checks.some(c => !c.ok))
            process.exitCode = 1;
        return;
    }
    if (command !== 'run')
        throw new Error('Unknown command; use --help');
    if (config.runtime.mode === 'docker' && process.platform !== 'linux')
        throw new Error('Docker production driver requires Linux/WSL2. Use local mode for native Windows tests.');
    const unlock = await lockDataDir(config.dataDir);
    let store: Store | undefined;
    let gateway: import('./gateway.ts').Gateway | undefined;
    let channel: ReturnType<typeof import('./feishu.ts').feishuTransport>['channel'] | undefined;
    let closed = false;
    const shutdown = async () => { if (closed)
        return; closed = true; log('shutdown'); try {
        await channel?.disconnect().catch(() => { });
        await gateway?.close();
    }
    finally {
        store?.close();
        await unlock();
    } };
    try {
        store = new Store(join(config.dataDir, 'gateway.sqlite'));
        const { feishuTransport, normalize } = await import('./feishu.ts');
        const { runtimeFactory, cleanupOwnedContainers } = await import('./runtime.ts');
        const { Gateway } = await import('./gateway.ts');
        const connected = feishuTransport(config, log);
        channel = connected.channel;
        gateway = new Gateway(config, store, connected.transport, runtimeFactory(config, secrets, log), secrets);
        process.once('SIGINT', () => void shutdown());
        process.once('SIGTERM', () => void shutdown());
        channel.on('message', async (message) => { try {
            await gateway!.message(normalize(message));
        }
        catch (e) {
            log('inbound_failed', { error: errorText(e) });
        } });
        channel.on('cardAction', async (event) => ({ toast: { type: 'info', content: await gateway!.approvals.click(event.chatId, event.operator.openId, event.action.value, event.messageId) } }));
        channel.on('error', error => log('feishu_error', { code: error.code }));
        channel.on('reconnecting', () => log('feishu_reconnecting'));
        channel.on('reconnected', () => log('feishu_reconnected'));
        await cleanupOwnedContainers(config);
        await channel.connect();
        await gateway.recover();
        log('ready', { mode: config.runtime.mode, groups: config.groups.length, dms: config.dms.length });
    }
    catch (error) {
        await shutdown();
        throw error;
    }
}
main().catch(error => { console.error(errorText(error)); process.exitCode = 1; });
