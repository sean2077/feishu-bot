#!/usr/bin/env node
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { loadConfig, modelHeaders, secretsFor } from './config.ts';
import { lockDataDir, redact, errorText } from './util.ts';
import { parseCli } from './cli-options.ts';
import { deploymentChecks, runDoctor } from './doctor.ts';
import { Store } from './store.ts';
let secrets: string[] = [];
async function main(): Promise<void> {
    const { command, configPath, probe } = parseCli(process.argv.slice(2));
    if (command === 'help') {
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
        console.log(`Created ${configPath}. Fill in app/chat/member IDs and model endpoint settings, then run npm run doctor and npm start.`);
        return;
    }
    const config = await loadConfig(configPath).catch(error => {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') throw new Error(`Missing ${configPath}. Run npm run init first.`);
        if (error instanceof SyntaxError) {
            // JSON parser errors may echo credential-bearing source snippets.
            const location = /line (\d+) column (\d+)/.exec(error.message);
            const position = /position (\d+)/.exec(error.message);
            const where = location ? ` at line ${location[1]}, column ${location[2]}` : position ? ` at position ${position[1]}` : '';
            throw new Error(`Invalid JSON in ${configPath}${where}. Check quotes, commas and escaped backslashes. Configuration content is not printed.`);
        }
        throw error;
    });
    secrets = secretsFor(config);
    const log = (event: string, detail: object = {}) => process.stderr.write(redact(JSON.stringify({ at: new Date().toISOString(), event, ...detail }), secrets) + '\n');
    if (command === 'doctor') {
        const report = await runDoctor(config, probe);
        console.log(JSON.stringify(report, null, 2));
        if (!report.ready) process.exitCode = 1;
        return;
    }
    const failures = deploymentChecks(config).filter(check => !check.ok);
    if (failures.length) throw new Error('Startup configuration is incomplete. Run npm run doctor.\n' + failures.map(check => `${check.name}: ${check.detail}`).join('\n'));
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
        finally { try { store?.close(); } finally { await unlock(); } }
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
        channel.on('cardAction', async event => {
            if (!event.messageId) return {};
            const content = await gateway!.approvals.click(event.chatId, event.operator.openId, event.action.value, event.messageId);
            return content ? { toast: { type: 'info' as const, content } } : {};
        });
        channel.on('error', error => log('feishu_error', { code: error.code }));
        channel.on('reconnecting', () => log('feishu_reconnecting'));
        channel.on('reconnected', () => log('feishu_reconnected'));
        // Recover persisted state before the event source can deliver new input.
        await gateway.recover();
        await channel.connect();
        log('ready', { mode: 'native', groups: config.groups.length, dms: config.dms.length });
    } catch (error) { await shutdown(); throw error; }
}
main().catch(error => { console.error(redact(errorText(error), secrets)); process.exitCode = 1; });
