import { execFile } from 'node:child_process';
import { performance } from 'node:perf_hooks';

const usageKeys = ['input_tokens', 'output_tokens', 'cache_read_input_tokens', 'cache_creation_input_tokens'];
export function reportedUsage(value) {
    return Object.fromEntries(usageKeys.map(key => [key,
        typeof value?.[key] === 'number' && Number.isFinite(value[key]) && value[key] >= 0 ? value[key] : null]));
}
export function distribution(values) {
    const sorted = values.filter(v => typeof v === 'number' && Number.isFinite(v)).sort((a, b) => a - b);
    if (!sorted.length) return { count: 0, min: null, median: null, max: null };
    const mid = Math.floor(sorted.length / 2);
    return { count: sorted.length, min: sorted[0], median: sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2, max: sorted.at(-1) };
}
export function descendants(rows, root) {
    const ids = new Set([root]);
    for (;;) {
        const size = ids.size;
        for (const row of rows) if (ids.has(row.ppid)) ids.add(row.pid);
        if (size === ids.size) return ids;
    }
}
export function memoryFromRows(rows, root, collectorPid) {
    if (!rows.some(row => row.pid === root)) throw new Error('root-not-observed');
    const included = descendants(rows, root), excluded = descendants(rows, collectorPid);
    const processes = rows.filter(row => included.has(row.pid) && !excluded.has(row.pid));
    if (processes.some(row => !Number.isSafeInteger(row.bytes) || row.bytes < 0)) throw new Error('invalid-memory-snapshot');
    const host = processes.find(row => row.pid === root);
    if (!host) throw new Error('root-excluded');
    return { hostBytes: host.bytes, descendantBytes: processes.filter(row => row.pid !== root).reduce((sum, row) => sum + row.bytes, 0),
        descendantCount: processes.length - 1 };
}

/** One finite, read-only snapshot. Never terminates or searches by process name. */
export async function memorySnapshot() {
    const started = performance.now();
    const windows = process.platform === 'win32';
    const method = windows ? 'Win32_Process.WorkingSetSize' : 'ps RSS (KiB converted to bytes)';
    let collectorPid;
    try {
        if (!windows && process.platform !== 'linux') throw new Error('unsupported-platform');
        const command = windows ? 'powershell.exe' : 'ps';
        const args = windows ? ['-NoProfile', '-NonInteractive', '-Command',
            "$ErrorActionPreference='Stop'; [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); @(Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,WorkingSetSize) | ConvertTo-Json -Compress"]
            : ['-eo', 'pid=,ppid=,rss='];
        const stdout = await new Promise((resolve, reject) => {
            const child = execFile(command, args, { timeout: 15000, maxBuffer: 1048576, windowsHide: true,
                env: { ...process.env, LC_ALL: 'C' } }, (error, output) => error ? reject(error) : resolve(output));
            collectorPid = child.pid;
        });
        const parsed = windows ? JSON.parse(String(stdout).replace(/^\uFEFF/, '')) : null;
        const rows = windows ? (Array.isArray(parsed) ? parsed : [parsed]).map(row => ({
            pid: Number(row.ProcessId), ppid: Number(row.ParentProcessId), bytes: row.WorkingSetSize == null ? NaN : Number(row.WorkingSetSize)
        })) : String(stdout).trim().split(/\r?\n/).map(line => {
            const [pid, ppid, kib] = line.trim().split(/\s+/).map(Number);
            return { pid, ppid, bytes: kib * 1024 };
        });
        if (rows.some(row => !Number.isSafeInteger(row.pid) || !Number.isSafeInteger(row.ppid))) throw new Error('invalid-process-snapshot');
        return { status: 'observed', method, collectionMs: Math.round(performance.now() - started), ...memoryFromRows(rows, process.pid, collectorPid) };
    } catch {
        return { status: 'unavailable', method, collectionMs: Math.round(performance.now() - started),
            hostBytes: null, descendantBytes: null, descendantCount: null, reason: 'snapshot-failed-or-unsupported' };
    }
}
