import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { killTree } from '../dist/process.js';

const workerPath = fileURLToPath(new URL('../dist/worker.js', import.meta.url));

// The init and stop frames share one stdin write: start() reaches the asynchronous
// action-relay listen before the parser handles stop. Late startup must not bind
// a relay or launch Claude after cancellation.
test('stopping during native initialization closes a late action relay without an error', { timeout: 25000 }, async () => {
    const root = await mkdtemp(join(tmpdir(), 'feishu-early-stop-'));
    const child = spawn(process.execPath, [workerPath], {
        cwd: root, stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true,
        detached: process.platform !== 'win32'
    });
    let stdout = '', stderr = '';
    child.stdout.setEncoding('utf8');
    child.stderr.setEncoding('utf8');
    child.stdout.on('data', chunk => { stdout += chunk; });
    child.stderr.on('data', chunk => { stderr += chunk; });
    const closed = new Promise((resolve, reject) => {
        child.once('error', reject);
        child.once('close', (code, signal) => resolve({ code, signal }));
    });
    const init = {
        type: 'init', sessionId: randomUUID(), workDir: root, homeDir: root,
        permissionMode: 'default', allowedTools: [], queueLimit: 8,
        model: { url: 'http://127.0.0.1:9', token: 'fixture-only', model: 'fixture-model', smallModel: 'fixture-model' },
        actions: [{ id: 'fixture_action', description: 'A local test action', parameters: {}, readOnly: true }]
    };
    let timer;
    try {
        child.stdin.end(JSON.stringify(init) + '\n' + JSON.stringify({ type: 'stop' }) + '\n');
        const outcome = await Promise.race([
            closed,
            new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('early-stop worker remained alive')), 15000); })
        ]);
        assert.equal(outcome.code, 0, JSON.stringify({ outcome, stdout, stderr }));
        assert.ok(!stdout.includes('"type":"ready"'), stdout);
        assert.ok(!stdout.includes('"type":"error"'), stdout);
        assert.ok(!stderr.includes('Claude worker failed'), stderr);
    } finally {
        clearTimeout(timer);
        await killTree(child, 100).catch(() => {});
        await rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
});
