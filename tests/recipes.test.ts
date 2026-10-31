import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { resolve, join } from 'node:path';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { setTimeout as delay } from 'node:timers/promises';
import { runCommand, cleanEnv } from '../src/actions.ts';
test('Jenkins recipe fixes job scope, uses service auth, does not retry writes', async () => {
    const requests: {
        url: string;
        method: string;
    }[] = [];
    const server = http.createServer((req, res) => {
        assert.equal(req.headers.authorization, 'Basic ' + Buffer.from('fixture:fixture-token').toString('base64'));
        requests.push({ url: req.url!, method: req.method! });
        if (req.method === 'GET') {
            res.writeHead(200, { 'content-type': 'application/json' });
            res.end(JSON.stringify({ number: 42, building: false, result: 'SUCCESS' }));
        }
        else {
            res.writeHead(503);
            res.end();
        }
    });
    await new Promise<void>(r => server.listen(0, '127.0.0.1', r));
    const address = server.address() as {
        port: number;
    };
    const env = cleanEnv({ BOT_ACTION_TEST_HTTP: '1', JENKINS_URL: `http://127.0.0.1:${address.port}`, JENKINS_JOB: 'team/project', JENKINS_USER: 'fixture', JENKINS_TOKEN: 'fixture-token' });
    const script = resolve('examples/actions/project-operations.mjs');
    try {
        const status = await runCommand(process.execPath, [script, 'jenkins-status'], { env });
        assert.equal(status.code, 0);
        assert.equal(JSON.parse(status.output).result, 'SUCCESS');
        assert.ok(requests[0]!.url.startsWith('/job/team/job/project/lastBuild/api/json?'));
        const build = await runCommand(process.execPath, [script, 'jenkins-build', 'main'], { env });
        assert.equal(build.code, 1);
        assert.match(build.output, /HTTP 503/);
        assert.equal(requests.filter(r => r.method === 'POST').length, 1);
        const invalid = await runCommand(process.execPath, [script, 'jenkins-build', '--bad'], { env });
        assert.equal(invalid.code, 1);
        assert.equal(requests.length, 2);
    }
    finally {
        server.closeAllConnections();
        await new Promise<void>(r => server.close(() => r()));
    }
});
test('POSIX action timeout terminates children holding pipes after their leader exits', { skip: process.platform === 'win32' }, async () => {
    const program = `const {spawn}=require('node:child_process');spawn(process.execPath,['-e','setTimeout(()=>{},30000)'],{stdio:'inherit'}).unref();`;
    const start = Date.now();
    await assert.rejects(runCommand(process.execPath, ['-e', program], { timeoutMs: 150 }), /timed out/);
    assert.ok(Date.now() - start < 7000);
});

test('action timeout reaps a live inherited-pipe child after its launcher exited', { timeout: 30000 }, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-action-orphan-'));
    const descendant = [
        "const fs = require('node:fs');",
        "const path = require('node:path');",
        "const dir = process.argv[1];",
        "fs.writeFileSync(path.join(dir, 'child.pid'), String(process.pid));",
        "let beat = 0;",
        "setInterval(() => fs.writeFileSync(path.join(dir, 'child.beat'), String(++beat)), 30);",
        "setTimeout(() => process.exit(0), 23000).unref();"
    ].join('\n');
    const launcher = [
        "const { spawn } = require('node:child_process');",
        "const fs = require('node:fs');",
        "const path = require('node:path');",
        "const dir = process.argv[1];",
        "spawn(process.execPath, ['-e', process.argv[2], dir], { stdio: 'inherit', windowsHide: true });",
        "const timer = setInterval(() => {",
        "  if (!fs.existsSync(path.join(dir, 'child.pid'))) return;",
        "  clearInterval(timer); fs.writeFileSync(path.join(dir, 'launcher.exited'), 'yes'); process.exit(0);",
        "}, 20);"
    ].join('\n');
    let childPid: number | undefined;
    try {
        // Inheriting the parent's pipe keeps runCommand open after its root exits.
        // A timeout must terminate this still-running descendant, not merely reject.
        await assert.rejects(
            runCommand(process.execPath, ['-e', launcher, dir, descendant], { timeoutMs: 2500 }),
            /timed out/
        );
        assert.equal(await readFile(join(dir, 'launcher.exited'), 'utf8'), 'yes');
        childPid = Number(await readFile(join(dir, 'child.pid'), 'utf8'));
        assert.ok(Number.isSafeInteger(childPid) && childPid > 0);
        const beat = await readFile(join(dir, 'child.beat'), 'utf8');
        await delay(180);
        assert.equal(await readFile(join(dir, 'child.beat'), 'utf8'), beat,
            'Timed-out action left its orphaned child writing after the launcher exited');
        if (process.platform === 'win32') {
            assert.throws(() => process.kill(childPid!, 0), { code: 'ESRCH' },
                'Windows must terminate the tracked child PID rather than only stopping its heartbeat');
        }
    } finally {
        // The PID comes only from this isolated fixture; never terminate by name.
        if (childPid) try { process.kill(childPid, 'SIGKILL'); } catch { /* already exited */ }
        await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
});
