import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { setTimeout as delay } from 'node:timers/promises';
import { killTree } from '../src/process.ts';

async function live(pid: number): Promise<boolean> {
    try { process.kill(pid, 0); }
    catch (error) { return (error as NodeJS.ErrnoException).code !== 'ESRCH'; }
    if (process.platform === 'linux') {
        try {
            const status = await readFile(`/proc/${pid}/stat`, 'utf8');
            // A killed orphan can remain a zombie until the container's init reaps it.
            return !['Z', 'X'].includes(status.slice(status.lastIndexOf(')') + 2, status.lastIndexOf(')') + 3));
        } catch (error) {
            // Between kill(pid, 0) and reading /proc, the process may exit.
            // Both errors confirm the child is no longer live; neither weakens the PID check.
            if (['ENOENT', 'ESRCH'].includes((error as NodeJS.ErrnoException).code ?? '')) return false;
            throw error;
        }
    }
    return true;
}
async function eventually(check: () => Promise<boolean>, message: string): Promise<void> {
    const deadline = Date.now() + 10000;
    while (Date.now() < deadline) { if (await check()) return; await delay(20); }
    assert.fail(message);
}

// Runs on Windows too: a call to taskkill/stop is not evidence of descendants exiting.
test('native process-tree stop terminates the parent, child and grandchild', { timeout: 25000 }, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu process 中文 '));
    const script = join(dir, 'tree.cjs');
    await writeFile(script, `
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const [dir, levelText] = process.argv.slice(2);
const level = Number(levelText);
const beat = path.join(dir, level + '.beat');
let count = 0;
fs.writeFileSync(path.join(dir, level + '.json'), JSON.stringify({pid: process.pid, ppid: process.ppid}));
fs.writeFileSync(beat, String(count));
setInterval(() => fs.writeFileSync(beat, String(++count)), 30);
// A safety net only for failed test cleanup, not the termination condition.
setTimeout(() => process.exit(0), 23000).unref();
if (level < 2) spawn(process.execPath, [__filename, dir, String(level + 1)], {stdio: 'ignore', windowsHide: true});
`);
    const parent = spawn(process.execPath, [script, dir, '0'], {
        stdio: 'ignore', detached: process.platform !== 'win32', windowsHide: true
    });
    const pids: number[] = [];
    let spawnError: Error | undefined;
    parent.once('error', error => { spawnError = error; });
    try {
        await eventually(async () => {
            if (spawnError) throw spawnError;
            try {
                const records = await Promise.all([0, 1, 2].map(i => readFile(join(dir, `${i}.json`), 'utf8').then(JSON.parse)));
                assert.equal(records[0].pid, parent.pid);
                assert.equal(records[1].ppid, parent.pid);
                assert.equal(records[2].ppid, records[1].pid);
                pids.splice(0, pids.length, ...records.map(r => Number(r.pid)));
                return pids.length === 3 && (await Promise.all(pids.map(live))).every(Boolean);
            } catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT' || error instanceof SyntaxError) return false; throw error; }
        }, 'Three live generations were not established');
        assert.equal(new Set(pids).size, 3);
        await killTree(parent, 100);
        await eventually(async () => (await Promise.all(pids.map(live))).every(value => !value),
            'A descendant remained alive after process-tree stop');
        const beats = await Promise.all([0, 1, 2].map(i => readFile(join(dir, `${i}.beat`), 'utf8')));
        await delay(120);
        assert.deepEqual(await Promise.all([0, 1, 2].map(i => readFile(join(dir, `${i}.beat`), 'utf8'))), beats,
            'Terminated descendants must no longer write heartbeats');
    } finally {
        // Only test-owned recorded PIDs, never a process name or broad system filter.
        await killTree(parent, 100).catch(() => {});
        for (const pid of pids) if (await live(pid)) try { process.kill(pid, 'SIGKILL'); } catch { /* already exited */ }
        await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
});
