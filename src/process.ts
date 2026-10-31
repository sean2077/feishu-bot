import { spawn, type ChildProcess } from 'node:child_process';
import { setTimeout as delay } from 'node:timers/promises';
export async function killTree(child: ChildProcess, graceMs = 1500): Promise<void> {
    if (!child.pid)
        return;
    if (process.platform === 'win32') {
        if (child.exitCode !== null || child.signalCode !== null)
            return;
        // Killing cmd.exe alone leaves the native agent and its grandchildren running.
        await new Promise<void>((resolve) => { const killer = spawn('taskkill', ['/PID', String(child.pid), '/T', '/F'], { windowsHide: true, stdio: 'ignore' }); killer.once('close', () => resolve()); killer.once('error', () => { child.kill(); resolve(); }); });
        return;
    }
    try {
        process.kill(-child.pid, 'SIGTERM');
    }
    catch {
        try {
            child.kill('SIGTERM');
        }
        catch {
            return;
        }
    }
    await Promise.race([new Promise<void>(resolve => child.once('close', () => resolve())), delay(graceMs)]);
    // Also kill descendants that survived their group leader; never use PID name matching.
    try {
        process.kill(-child.pid, 'SIGKILL');
    }
    catch { /* process group gone */ }
}
export class JsonLines {
    private buffer = '';
    readonly max: number;
    readonly consume: (value: Record<string, unknown>) => void;
    constructor(consume: (value: Record<string, unknown>) => void, max = 2 * 1024 * 1024) { this.consume = consume; this.max = max; }
    push(chunk: string): void {
        this.buffer += chunk;
        for (;;) {
            const i = this.buffer.indexOf('\n');
            if (i < 0)
                break;
            if (i > this.max)
                throw new Error('protocol frame too large');
            const line = this.buffer.slice(0, i);
            this.buffer = this.buffer.slice(i + 1);
            if (!line.trim())
                continue;
            const value: unknown = JSON.parse(line);
            if (!value || typeof value !== 'object' || Array.isArray(value))
                throw new Error('invalid protocol frame');
            this.consume(value as Record<string, unknown>);
        }
        if (this.buffer.length > this.max)
            throw new Error('protocol frame too large');
    }
}
