import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { resolve } from 'node:path';
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
