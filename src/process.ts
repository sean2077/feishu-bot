import { spawn, execFile, type ChildProcess } from 'node:child_process';
import { setTimeout as delay } from 'node:timers/promises';
import { promisify } from 'node:util';
import { join } from 'node:path';
const execFileAsync = promisify(execFile);

type ProcessLifetime = { startedAt: number; exitedAt?: number };
const lifetimes = new WeakMap<ChildProcess, ProcessLifetime>();

/** Timings are opt-in, process-identity-free observations, not proof of cleanup. */
export interface WindowsCleanupReport {
    method: 'taskkill' | 'cim';
    outcome: 'success' | 'failure';
    elapsedMs: number;
    stages: { name: 'started' | 'snapshot' | 'ancestry' | 'termination'; elapsedMs: number }[];
}
type WindowsCleanupObserver = (report: WindowsCleanupReport) => void;
function cimStages(output: string): WindowsCleanupReport['stages'] {
    const stages: WindowsCleanupReport['stages'] = [];
    for (const match of output.matchAll(/^FBCIM\|(started|snapshot|ancestry|termination)\|(\d+)\r?$/gm)) {
        const name = match[1] as WindowsCleanupReport['stages'][number]['name'];
        const elapsedMs = Number(match[2]);
        if (Number.isSafeInteger(elapsedMs) && elapsedMs >= (stages.at(-1)?.elapsedMs ?? 0))
            stages.push({ name, elapsedMs });
    }
    return stages;
}

/** Preserve the original root's lifetime to guard against recycled Windows PIDs. */
export function trackProcessTree(child: ChildProcess, startedAt = Date.now()): void {
    const lifetime: ProcessLifetime = { startedAt };
    lifetimes.set(child, lifetime);
    child.once('exit', () => { lifetime.exitedAt = Date.now(); });
}

/** Win32_Process retains a parent PID after its owner exits, but PIDs can be reused.
 * A one-shot CIM snapshot can recover a still-connected orphan tree. It cannot
 * safely reconstruct a chain if intermediate ancestors have already disappeared.
 * Never terminate processes by executable name or an unverified PID alone.
 */
async function killWindowsOrphans(rootPid: number, lifetime: ProcessLifetime, observe?: WindowsCleanupObserver): Promise<void> {
    const script = [
        "$ErrorActionPreference = 'Stop'",
        '$watch = [Diagnostics.Stopwatch]::StartNew()',
        "Write-Output ('FBCIM|started|' + $watch.ElapsedMilliseconds)",
        '$rootId = ' + rootPid,
        "$startedAt = [datetime]::Parse('" + new Date(lifetime.startedAt).toISOString() + "', [cultureinfo]::InvariantCulture, [globalization.DateTimeStyles]::RoundtripKind)",
        "$exitedAt = ([datetime]::Parse('" + new Date(lifetime.exitedAt ?? Date.now()).toISOString() + "', [cultureinfo]::InvariantCulture, [globalization.DateTimeStyles]::RoundtripKind)).AddMilliseconds(500)",
        '$all = @(Get-CimInstance -ClassName Win32_Process -Property ProcessId,ParentProcessId,CreationDate)',
        "Write-Output ('FBCIM|snapshot|' + $watch.ElapsedMilliseconds)",
        '$depth = @{}; $birth = @{}; $matches = @()',
        '$depth[$rootId] = 0; $birth[$rootId] = $startedAt',
        'do {',
        '    $changed = $false',
        '    foreach ($item in $all) {',
        '        $id = [int]$item.ProcessId; $parent = [int]$item.ParentProcessId',
        '        if ($id -eq $rootId -or $depth.ContainsKey($id) -or -not $depth.ContainsKey($parent) -or $null -eq $item.CreationDate) { continue }',
        '        $created = $item.CreationDate.ToUniversalTime()',
        '        if ($created -lt $birth[$parent] -or $created -lt $startedAt -or ($parent -eq $rootId -and $created -gt $exitedAt)) { continue }',
        '        $depth[$id] = $depth[$parent] + 1; $birth[$id] = $created',
        '        $matches += $item; $changed = $true',
        '    }',
        '} while ($changed)',
        "Write-Output ('FBCIM|ancestry|' + $watch.ElapsedMilliseconds)",
        'foreach ($item in ($matches | Sort-Object -Property @{ Expression = { $depth[[int]$_.ProcessId] }; Descending = $true })) {',
        '    $native = Get-Process -Id ([int]$item.ProcessId) -ErrorAction SilentlyContinue',
        '    if ($null -eq $native) { continue }',
        '    try {',
        '        if ([math]::Abs(($native.StartTime.ToUniversalTime() - $item.CreationDate.ToUniversalTime()).TotalMilliseconds) -gt 1000) { continue }',
        '        $native.Kill()',
        '        if (-not $native.WaitForExit(2000)) { throw "Worker descendant did not exit" }',
        '    } catch [System.InvalidOperationException] { }',
        '}',
        "Write-Output ('FBCIM|termination|' + $watch.ElapsedMilliseconds)",
    ].join('\n');
    const executable = join(process.env.SystemRoot ?? 'C:\\Windows', 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe');
    const startedAt = performance.now();
    try {
        const { stdout } = await execFileAsync(executable, ['-NoProfile', '-NonInteractive', '-Command', script],
            { windowsHide: true, timeout: 15000, maxBuffer: 64 * 1024 });
        try { observe?.({ method: 'cim', outcome: 'success', elapsedMs: Math.round(performance.now() - startedAt),
            stages: cimStages(stdout) }); } catch { /* Diagnostics cannot affect termination. */ }
    } catch (error) {
        const partial = (error as Error & { stdout?: string }).stdout ?? '';
        const stages = cimStages(partial);
        try { observe?.({ method: 'cim', outcome: 'failure', elapsedMs: Math.round(performance.now() - startedAt),
            stages }); } catch { /* Diagnostics cannot affect termination. */ }
        const last = stages.at(-1)?.name ?? 'not_started';
        const active = last === 'started' ? 'snapshot' : last === 'snapshot' ? 'ancestry'
            : last === 'ancestry' ? 'termination' : last === 'termination' ? 'process_exit' : 'powershell_startup';
        throw new Error(`Windows orphan cleanup failed (phase=${active}, last=${last}, elapsedMs=${Math.round(performance.now() - startedAt)})`,
            { cause: error });
    }
}

export async function killTree(child: ChildProcess, graceMs = 1500, onWindowsCleanup?: WindowsCleanupObserver): Promise<void> {
    if (!child.pid)
        return;
    if (process.platform === 'win32') {
        if (child.exitCode === null && child.signalCode === null) {
            // Prefer taskkill /T while the original root is still alive.
            const taskkillStartedAt = performance.now();
            const killed = await new Promise<boolean>(resolve => {
                const killer = spawn('taskkill', ['/PID', String(child.pid), '/T', '/F'], { windowsHide: true, stdio: 'ignore' });
                killer.once('close', code => resolve(code === 0));
                killer.once('error', () => resolve(false));
            });
            try { onWindowsCleanup?.({ method: 'taskkill', outcome: killed ? 'success' : 'failure',
                elapsedMs: Math.round(performance.now() - taskkillStartedAt), stages: [] }); }
            catch { /* Optional diagnostic hooks cannot change cleanup. */ }
            if (killed) return;
            try { child.kill(); } catch { /* Root may already have exited. */ }
        }
        // When the parent is gone, taskkill /T cannot discover its children.
        // Only the lifetime registered at spawn authorizes a targeted fallback.
        const lifetime = lifetimes.get(child);
        if (lifetime) await killWindowsOrphans(child.pid, lifetime, onWindowsCleanup);
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
