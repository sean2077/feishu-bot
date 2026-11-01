import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, readdir, rm, mkdir, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import http from 'node:http';
import { parseConfig } from '../src/config.ts';
import { parseCli } from '../src/cli-options.ts';
import { deploymentChecks, runDoctor } from '../src/doctor.ts';

const root = fileURLToPath(new URL('../', import.meta.url));
const template = () => readFile(join(root, 'examples/config.example.json'), 'utf8').then(JSON.parse);
const base = (dir: string) => ({ tenantId: 'tenant_fixture', appId: 'cli_fixture', dataDir: dir,
    appSecretEnv: 'DOCTOR_FIXTURE_APP', model: { baseUrl: 'https://endpoint.test', model: 'fixture-model', tokenEnv: 'DOCTOR_FIXTURE_MODEL' },
    projects: [{ id: 'assistant', maintainers: ['ou_fixture'] }], groups: [], dms: [{ userId: 'ou_fixture', projectId: 'assistant' }] });
async function environment(run: () => Promise<void>): Promise<void> {
    const before = { ...process.env };
    process.env.DOCTOR_FIXTURE_APP = 'fixture-app-not-a-real-secret';
    process.env.DOCTOR_FIXTURE_MODEL = 'fixture-model-not-a-real-secret';
    try { await run(); } finally { process.env = before; }
}
const cli = (cwd: string, args: string[]) => spawnSync(process.execPath, ['--experimental-strip-types', join(root, 'src/cli.ts'), ...args],
    { cwd, encoding: 'utf8', env: process.env, timeout: 15000 });

test('CLI rejects a missing config path instead of using a default or another flag', () => {
    for (const args of [['doctor', '--config'], ['run', '--config', '--probe'], ['init', '--config='], ['doctor', '--config', '   ']])
        assert.throws(() => parseCli(args), /requires a file path/);
    assert.throws(() => parseCli(['doctor', '--config', 'one', '--config=two']), /only be specified once/);
    assert.throws(() => parseCli(['run', '--probe']), /only by doctor/);
    assert.throws(() => parseCli(['unexpected']), /Unknown command/);
    assert.equal(parseCli([]).command, 'help');
    assert.equal(parseCli(['--help']).command, 'help');
    assert.equal(parseCli(['doctor', '--config=folder with spaces/config.json', '--probe']).configPath, resolve('folder with spaces/config.json'));
});

test('unfilled templates and missing bindings cannot pass deployment checks', async () => {
    await environment(async () => {
        const raw = await template();
        raw.appSecretEnv = 'DOCTOR_FIXTURE_APP'; raw.model.tokenEnv = 'DOCTOR_FIXTURE_MODEL';
        const checks = deploymentChecks(parseConfig(raw));
        assert.equal(checks.find(c => c.name === 'configuration values')!.ok, false);
        assert.match(checks[0]!.detail!, /appId.*model\.model.*model\.baseUrl/);
        const noRoutes = parseConfig({ ...base('state'), dms: [] });
        assert.equal(deploymentChecks(noRoutes).find(c => c.name === 'chat bindings')!.ok, false);
        const valid = deploymentChecks(parseConfig(base('state')));
        assert.ok(valid.every(c => c.ok));
        process.env.DOCTOR_FIXTURE_MODEL = 'replace-with-model-token';
        assert.equal(deploymentChecks(parseConfig(base('state'))).find(c => c.name === 'credential DOCTOR_FIXTURE_MODEL')!.ok, false);
    });
});

test('doctor aggregates directory and resource errors instead of aborting the report', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-doctor-'));
    try {
        await environment(async () => {
            const notDir = join(dir, 'file'); await writeFile(notDir, 'untouched');
            const raw = base(join(notDir, 'state'));
            const config = parseConfig({ ...raw, projects: [{ ...raw.projects[0], seedDir: join(dir, 'missing') }] });
            const report = await runDoctor(config);
            assert.equal(report.ready, false);
            assert.equal(report.checks.find(c => c.name === 'data directory / SQLite')!.ok, false);
            assert.equal(report.checks.find(c => c.name === 'assistant seedDir')!.ok, false);
            assert.ok(report.checks.find(c => c.name === 'Git'));
            assert.equal(await readFile(notDir, 'utf8'), 'untouched');
        });
    } finally { await rm(dir, { recursive: true, force: true }); }
});

test('doctor cleans its unique SQLite probe and never opens existing state', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-doctor-'));
    try {
        await environment(async () => {
            for (const name of ['doctor.sqlite', 'gateway.sqlite', 'gateway.lock']) await writeFile(join(dir, name), 'do-not-open');
            const config = parseConfig(base(dir));
            const reports = await Promise.all([runDoctor(config), runDoctor(config)]);
            assert.ok(reports.every(r => r.ready));
            assert.deepEqual((await readdir(dir)).sort(), ['doctor.sqlite', 'gateway.lock', 'gateway.sqlite']);
            for (const name of await readdir(dir)) assert.equal(await readFile(join(dir, name), 'utf8'), 'do-not-open');
            assert.match(reports[0]!.scope, /no Feishu or model requests/);
        });
    } finally { await rm(dir, { recursive: true, force: true }); }
});

test('source CLI refuses incomplete run before opening state and diagnoses templates with a failing exit', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-cli-'));
    try {
        await environment(async () => {
            const raw = await template(); raw.dataDir = join(dir, 'state');
            raw.appSecretEnv = 'DOCTOR_FIXTURE_APP'; raw.model.tokenEnv = 'DOCTOR_FIXTURE_MODEL';
            await writeFile(join(dir, 'config.json'), JSON.stringify(raw));
            const missing = cli(dir, ['doctor', '--config']);
            assert.equal(missing.status, 1); assert.match(missing.stderr, /requires a file path/);
            assert.equal(missing.stdout, '');
            const run = cli(dir, ['run']);
            assert.equal(run.status, 1); assert.match(run.stderr, /configuration is incomplete/);
            assert.deepEqual((await readdir(dir)).sort(), ['config.json']);
            const report = cli(dir, ['doctor']);
            assert.equal(report.status, 1, report.stderr);
            assert.equal(JSON.parse(report.stdout).ready, false);
            assert.deepEqual(await readdir(join(dir, 'state')), []);
            assert.ok(!report.stdout.includes(process.env.DOCTOR_FIXTURE_MODEL!));
        });
    } finally { await rm(dir, { recursive: true, force: true }); }
});

test('init validates arguments before creating files and never overwrites a configuration', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-init-'));
    try {
        const invalid = cli(dir, ['init', '--config']);
        assert.equal(invalid.status, 1); assert.deepEqual(await readdir(dir), []);
        const path = join(dir, '中文 directory', 'config.json');
        assert.equal(cli(dir, ['init', '--config', path]).status, 0);
        await writeFile(path, 'keep-my-config');
        const again = cli(dir, ['init', '--config', path]);
        assert.equal(again.status, 1); assert.match(again.stderr, /not overwritten/);
        assert.equal(await readFile(path, 'utf8'), 'keep-my-config');
    } finally { await rm(dir, { recursive: true, force: true }); }
});

test('opt-in model probe rejects HTTP 200 HTML and skips incomplete configuration without a request', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-probe-'));
    let count = 0, valid = false;
    const server = http.createServer((req, res) => {
        count++; req.resume();
        res.writeHead(200, { 'content-type': valid ? 'application/json' : 'text/html' });
        res.end(valid ? JSON.stringify({ type: 'message', role: 'assistant', content: [{ type: 'text', text: 'OK' }] }) : '<html>Login</html>');
    });
    await new Promise<void>(r => server.listen(0, '127.0.0.1', r));
    try {
        await environment(async () => {
            const raw = base(dir);
            const config = parseConfig({ ...raw, model: { ...raw.model, baseUrl: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`, allowHttp: true } });
            config.appId = 'cli_example_app';
            const skipped = await runDoctor(config, true);
            assert.equal(count, 0); assert.equal(skipped.ready, false);
            assert.equal(skipped.checks.at(-1)!.skipped, true);
            assert.match(skipped.scope, /no Feishu or model requests/);
            config.appId = 'cli_fixture';
            const invalid = await runDoctor(config, true);
            assert.equal(count, 1); assert.equal(invalid.ready, false);
            assert.match(invalid.checks.at(-1)!.detail!, /not a JSON Messages/);
            valid = true;
            const success = await runDoctor(config, true);
            assert.equal(count, 2); assert.equal(success.ready, true);
            assert.match(success.scope, /not full deployment acceptance/);
        });
    } finally { server.closeAllConnections(); await new Promise<void>(r => server.close(() => r())); await rm(dir, { recursive: true, force: true }); }
});

test('header diagnostics reveal variable names but not sensitive values', async () => {
    await environment(async () => {
        const raw = base('state');
        process.env.DOCTOR_HEADER_FIXTURE = 'secret-fixture\r\ninjected: value';
        const config = parseConfig({ ...raw, model: { ...raw.model, extraHeaders: { 'x-token': 'env:DOCTOR_HEADER_FIXTURE' } } });
        const checks = deploymentChecks(config);
        assert.equal(checks.find(c => c.name === 'model endpoint headers')!.ok, false);
        assert.ok(!JSON.stringify(checks).includes('secret-fixture'));
        process.env.DOCTOR_HEADER_FIXTURE = 'replace-with-header';
        assert.equal(deploymentChecks(config).find(c => c.name === 'model endpoint headers')!.ok, false);
    });
});


test('malformed configuration reports a location without echoing raw credential-bearing content', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-json-'));
    try {
        await writeFile(join(dir, 'config.json'), '{"model":{"token":"private-fixture-value-not-for-output"},BROKEN}');
        const result = cli(dir, ['doctor']);
        assert.equal(result.status, 1);
        assert.match(result.stderr, /Invalid JSON/);
        assert.match(result.stderr, /line \d+, column \d+|position \d+/);
        assert.match(result.stderr, /quotes, commas/);
        assert.ok(!result.stderr.includes('private-fixture-value-not-for-output'));
        assert.ok(!result.stderr.includes('BROKEN'));
        assert.deepEqual(await readdir(dir), ['config.json']);
    } finally { await rm(dir, { recursive: true, force: true }); }
});

test('Windows doctor rejects an existing executable that is not Git Bash', { skip: process.platform !== 'win32' }, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-bash-doctor-'));
    try {
        await environment(async () => {
            // node.exe exists and is executable, so a stat-only check produces
            // a false "ready" for a setting that cannot launch Git Bash.
            process.env.CLAUDE_CODE_GIT_BASH_PATH = process.execPath;
            const report = await runDoctor(parseConfig(base(dir)));
            const bash = report.checks.find(c => c.name === 'configured Git Bash');
            assert.equal(bash?.ok, false, 'an existing non-Bash executable must fail the Git Bash check');
            assert.equal(report.ready, false);
            const pretender = join(dir, 'bash.exe');
            await writeFile(pretender, 'not an executable');
            process.env.CLAUDE_CODE_GIT_BASH_PATH = pretender;
            const invalidShell = await runDoctor(parseConfig(base(dir)));
            assert.equal(invalidShell.checks.find(c => c.name === 'configured Git Bash')?.ok, false,
                'a non-executable file named bash.exe must not pass either');
            const installedBash = join(process.env.ProgramFiles ?? 'C:\\Program Files', 'Git', 'bin', 'bash.exe');
            if ((await stat(installedBash).catch(() => undefined))?.isFile()) {
                // Portable installations remain supported: the configured path
                // is tested directly rather than assumed to be on PATH.
                process.env.CLAUDE_CODE_GIT_BASH_PATH = installedBash;
                const validShell = await runDoctor(parseConfig(base(dir)));
                assert.equal(validShell.checks.find(c => c.name === 'configured Git Bash')?.ok, true);
            }
        });
    } finally { await rm(dir, { recursive: true, force: true }); }
});
