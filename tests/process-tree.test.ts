import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { setTimeout as delay } from 'node:timers/promises';
import { killTree, trackProcessTree } from '../src/process.ts';
import { runCommand } from '../src/actions.ts';

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

// Parent exit must not leave an unowned native CLI process tree running.
test('native process-tree stop reaps live descendants after their root exited', { timeout: 40000 }, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-orphan-tree-'));
    const script = join(dir, 'orphan.cjs');
    await writeFile(script, [
        "const fs = require('node:fs');",
        "const path = require('node:path');",
        "const { spawn } = require('node:child_process');",
        "const [dir, text] = process.argv.slice(2);",
        "const level = Number(text);",
        "fs.writeFileSync(path.join(dir, level + '.json'), JSON.stringify({ pid: process.pid, ppid: process.ppid }));",
        "let beat = 0; fs.writeFileSync(path.join(dir, level + '.beat'), String(beat));",
        "setInterval(() => fs.writeFileSync(path.join(dir, level + '.beat'), String(++beat)), 30);",
        "setTimeout(() => process.exit(0), 23000).unref();",
        "if (level < 2) spawn(process.execPath, [__filename, dir, String(level + 1)], { stdio: 'ignore', windowsHide: true, detached: process.platform === 'win32' });",
        "if (level === 0) { const wait = setInterval(() => { if (fs.existsSync(path.join(dir, '2.json'))) { clearInterval(wait); process.exit(0); } }, 20); }"
    ].join('\n'));
    const bornAt = Date.now();
    const parent = spawn(process.execPath, [script, dir, '0'], {
        stdio: 'ignore', detached: process.platform !== 'win32', windowsHide: true
    });
    trackProcessTree(parent, bornAt);
    const pids: number[] = [];
    const closed = new Promise<void>((resolve, reject) => { parent.once('error', reject); parent.once('close', () => resolve()); });
    try {
        await closed;
        const records = await Promise.all([0, 1, 2].map(i => readFile(join(dir, `${i}.json`), 'utf8').then(JSON.parse)));
        assert.equal(records[0].pid, parent.pid);
        assert.equal(records[1].ppid, parent.pid);
        assert.equal(records[2].ppid, records[1].pid);
        pids.push(...records.map(r => Number(r.pid)));
        assert.equal(new Set(pids).size, 3);
        assert.ok(!(await live(pids[0]!)), 'root has exited before cleanup');
        await eventually(async () => {
            try { await Promise.all([1, 2].map(i => readFile(join(dir, `${i}.beat`)))); return true; }
            catch (e) { if ((e as NodeJS.ErrnoException).code === 'ENOENT') return false; throw e; }
        }, 'Descendant heartbeats were not established');
        const preLive = await Promise.all(pids.slice(1).map(live));
        const initialBeats = await Promise.all([1, 2].map(i => readFile(join(dir, `${i}.beat`), 'utf8')));
        await delay(180);
        const activeBeats = await Promise.all([1, 2].map(i => readFile(join(dir, `${i}.beat`), 'utf8')));
        assert.ok(preLive.every(Boolean) && activeBeats.every((beat, i) => beat !== initialBeats[i]),
            JSON.stringify({ preLive, initialBeats, activeBeats, pids }));
        await killTree(parent, 100);
        await eventually(async () => (await Promise.all(pids.slice(1).map(live))).every(value => !value),
            'Orphaned native descendants survived after the root exited');
        const beats = await Promise.all([1, 2].map(i => readFile(join(dir, `${i}.beat`), 'utf8')));
        await delay(120);
        assert.deepEqual(await Promise.all([1, 2].map(i => readFile(join(dir, `${i}.beat`), 'utf8'))), beats);
    } finally {
        await killTree(parent, 100).catch(() => {});
        for (const pid of pids) if (await live(pid)) try { process.kill(pid, 'SIGKILL'); } catch { /* exited */ }
        await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
});

 
// An external project action can outlive its original launcher while retaining its
// stdout pipe. Cancellation must still reclaim that process, not just the exited root.
test('cancelled action reclaims a surviving child after its launcher exits', { timeout: 40000 }, async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-action-orphan-'));
    const launcher = join(dir, 'launcher.cjs');
    const ticker = join(dir, 'ticker.cjs');
    await writeFile(ticker, [
        "const fs = require('node:fs');",
        "const path = require('node:path');",
        "const dir = process.argv[2];",
        "fs.writeFileSync(path.join(dir, 'child.pid'), String(process.pid));",
        "let n = 0;",
        "setInterval(() => fs.writeFileSync(path.join(dir, 'child.beat'), String(++n)), 30);",
        "setTimeout(() => process.exit(0), 23000).unref();"
    ].join('\n'));
    await writeFile(launcher, [
        "const fs = require('node:fs');",
        "const path = require('node:path');",
        "const { spawn } = require('node:child_process');",
        "const dir = process.argv[2];",
        "fs.writeFileSync(path.join(dir, 'root.pid'), String(process.pid));",
        "spawn(process.execPath, [path.join(dir, 'ticker.cjs'), dir], { stdio: ['ignore', 'inherit', 'inherit'], windowsHide: true, detached: process.platform === 'win32' });",
        "const timer = setInterval(() => { if (fs.existsSync(path.join(dir, 'child.beat'))) { clearInterval(timer); fs.writeFileSync(path.join(dir, 'root.exiting'), 'yes'); process.exit(0); } }, 20);"
    ].join('\n'));
    const controller = new AbortController();
    const execution = runCommand(process.execPath, [launcher, dir], { signal: controller.signal, timeoutMs: 20000 });
    void execution.catch(() => {}); // The test checks its outcome after the root has exited.
    let childPid = 0;
    try {
        await eventually(async () => {
            try {
                const [rootText, childText, exiting] = await Promise.all([
                    readFile(join(dir, 'root.pid'), 'utf8'), readFile(join(dir, 'child.pid'), 'utf8'),
                    readFile(join(dir, 'root.exiting'), 'utf8')
                ]);
                childPid = Number(childText);
                const rootPid = Number(rootText);
                // A Windows handle retained for stdout can keep kill(pid, 0) positive
                // even after the launcher has called process.exit().
                return rootPid > 0 && childPid > 0 && exiting === 'yes' &&
                    (process.platform === 'win32' || !(await live(rootPid))) && await live(childPid);
            } catch (error) {
                if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false;
                throw error;
            }
        }, 'Action launcher did not exit with a live descendant').catch(async error => {
            const paths = ['root.pid', 'child.pid', 'child.beat', 'root.exiting'];
            const files = Object.fromEntries(await Promise.all(paths.map(async name => [name,
                await readFile(join(dir, name), 'utf8').catch(e => String((e as NodeJS.ErrnoException).code ?? e))
            ])));
            throw new Error(String(error) + ': ' + JSON.stringify(files));
        });
        await delay(250); // Allow the root exit to settle on Windows before cancellation.
        const first = await readFile(join(dir, 'child.beat'), 'utf8');
        await delay(150);
        assert.notEqual(await readFile(join(dir, 'child.beat'), 'utf8'), first, 'Child was not actually running');
        controller.abort();
        await assert.rejects(execution, /operation cancelled/);
        await eventually(async () => !(await live(childPid)), 'Cancelled action left its descendant running');
        const stoppedBeat = await readFile(join(dir, 'child.beat'), 'utf8');
        await delay(120);
        assert.equal(await readFile(join(dir, 'child.beat'), 'utf8'), stoppedBeat);
    } finally {
        controller.abort();
        await execution.catch(() => {});
        if (childPid && await live(childPid)) try { process.kill(childPid, 'SIGKILL'); } catch { /* test-owned process exited */ }
        await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    }
});