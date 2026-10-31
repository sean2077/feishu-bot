import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, symlink, rm, mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { parseConfig } from '../src/config.ts';
import { actionArguments, runCommand } from '../src/actions.ts';
import { AsyncQueue, readBoundedFile, redact, lockDataDir } from '../src/util.ts';
import { JsonLines } from '../src/process.ts';
import { route } from '../src/gateway.ts';
import { fixture, message, rawConfig, until } from './helpers.ts';
import type { ActionSpec } from '../src/types.ts';
test('strict configuration rejects misspelled controls and unsafe model URLs', () => {
    assert.throws(() => parseConfig({ ...rawConfig('/tmp/x'), adminz: ['a'] }), /unknown field/);
    assert.throws(() => parseConfig({ ...rawConfig('/tmp/x'), model: { baseUrl: 'http://company', model: 'd' } }), /HTTPS/);
    assert.throws(() => parseConfig({ ...rawConfig('/tmp/x'), model: { baseUrl: 'https://secret:password@company', model: 'd' } }), /credentials/);
    assert.throws(() => parseConfig({ ...rawConfig('/tmp/x'), projects: [{ id: 'p', maintainers: [], actions: [] }] }), /maintainers/);
    assert.throws(() => parseConfig({ ...rawConfig('/tmp/x'), projects: [{ id: 'p', maintainers: ['a'], autoApproveWorkspace: true }] }), /local mode/);
});
test('a changed project binding changes the physical conversation namespace', () => {
    const c = parseConfig(rawConfig('/tmp/x'));
    const old = route(c, message())!.key;
    c.projects.push({ ...c.projects[0]!, id: 'p2' });
    c.groups[0]!.projectId = 'p2';
    assert.notEqual(route(c, message())!.key, old);
});
test('fixed argv interpolation does not invoke a shell or accept extra parameters', async () => {
    const spec: ActionSpec = { id: 'echo', description: 'echo', executable: process.execPath, args: ['-e', 'process.stdout.write(process.argv[1])', '{{value}}'], parameters: { value: { maxLength: 200 } }, env: {}, readOnly: true, timeoutMs: 1000 };
    const value = 'a; echo INJECTION && $(whoami)';
    const args = actionArguments(spec, { value });
    const result = await runCommand(process.execPath, args);
    assert.equal(result.output, value);
    assert.throws(() => actionArguments(spec, { value: '--version' }), /invalid/);
    assert.throws(() => actionArguments(spec, { value: 'ok', command: 'rm' }), /unknown/);
});
test('command output and runtime have enforced bounds', async () => {
    await assert.rejects(() => runCommand(process.execPath, ['-e', 'process.stdout.write("x".repeat(100000))'], { maxBytes: 1000 }), /size/);
    await assert.rejects(() => runCommand(process.execPath, ['-e', 'setInterval(()=>{},100)'], { timeoutMs: 50 }), /timed out/);
});
test('an action cannot execute before an authenticated approval', async () => {
    const f = await fixture();
    try {
        const output = join(f.dir, 'executed');
        f.config.projects[0]!.actions.push({ id: 'write', description: 'write marker', executable: process.execPath, args: ['-e', 'require("fs").writeFileSync(process.argv[1],"done")', output], env: {}, readOnly: false, timeoutMs: 5000, parameters: {} });
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'request', kind: 'action', id: 'action-test', name: 'write', input: {} });
        await until(() => !!f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get());
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM action_runs').get()!.n, 0);
        const id = f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id as string;
        await f.gateway.approvals.click('group1', 'admin', { request: id, decision: 'allow' });
        await until(() => w.responses.length === 1);
        assert.equal(w.responses[0]!.value.ok, true);
        assert.equal(f.store.db.prepare('SELECT state FROM action_runs').get()!.state, 'done');
    }
    finally {
        await f.close();
    }
});
test('question answers require explicit request identity; normal conversation is not consumed', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const w = f.workers[0]!;
        w.emit({ type: 'request', kind: 'question', id: 'q', name: 'AskUserQuestion', input: { questions: [{ question: 'Which branch?', options: [{ label: 'main' }, { label: 'dev' }] }] } });
        await until(() => !!f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get());
        const id = f.store.db.prepare("SELECT id FROM requests WHERE state='pending'").get()!.id as string;
        await f.gateway.message(message({ senderId: 'bob', text: 'some other comment' }));
        await until(() => w.sent.length === 2);
        assert.equal(w.responses.length, 0);
        await f.gateway.message(message({ text: `/answer ${id} release` }));
        await until(() => w.responses.length === 1);
        assert.deepEqual(w.responses[0]!.value.answers, { 'Which branch?': 'release' });
    }
    finally {
        await f.close();
    }
});
test('bounded file exports reject traversal, symlink escape and oversized content', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'export-'));
    try {
        const work = join(dir, 'work');
        await mkdir(work);
        await writeFile(join(dir, 'secret'), 'private');
        await writeFile(join(work, 'large'), 'x'.repeat(200));
        await assert.rejects(() => readBoundedFile(work, '../secret', 100), /outside/);
        await assert.rejects(() => readBoundedFile(work, 'large', 100), /policy/);
        if (process.platform !== 'win32') {
            await symlink(join(dir, 'secret'), join(work, 'link'));
            await assert.rejects(() => readBoundedFile(work, 'link', 100), /outside/);
        }
    }
    finally {
        await rm(dir, { recursive: true, force: true });
    }
});
test('data directory lock fails closed rather than stealing a running instance', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'lock-'));
    try {
        const release = await lockDataDir(dir);
        await assert.rejects(() => lockDataDir(dir), /locked/);
        await release();
        const again = await lockDataDir(dir);
        await again();
    }
    finally {
        await rm(dir, { recursive: true, force: true });
    }
});
test('bounded native input queue remains open between turns', async () => {
    const queue = new AsyncQueue<string>(2), iterator = queue[Symbol.asyncIterator]();
    queue.push('a');
    queue.push('b');
    assert.throws(() => queue.push('c'), /backpressure/);
    assert.equal((await iterator.next()).value, 'a');
    assert.equal((await iterator.next()).value, 'b');
    const next = iterator.next();
    queue.push('new turn');
    assert.equal((await next).value, 'new turn');
    queue.end();
    assert.equal((await iterator.next()).done, true);
});
test('framed process protocol rejects malformed, oversized and array payloads', () => {
    const frames: object[] = [];
    const parser = new JsonLines(v => frames.push(v), 64);
    parser.push('{"a":');
    parser.push('1}\n{"b":2}\n');
    assert.deepEqual(frames, [{ a: 1 }, { b: 2 }]);
    assert.throws(() => new JsonLines(() => { }).push('[]\n'), /invalid/);
    assert.throws(() => new JsonLines(() => { }, 10).push('x'.repeat(11)), /too large/);
});
test('redaction removes configured secrets and common authorization forms', () => {
    const output = redact('key=secretvalue Bearer abcd token=hello', ['secretvalue']);
    assert.ok(!output.includes('secretvalue'));
    assert.ok(!output.includes('abcd'));
    assert.ok(!output.includes('hello'));
});
