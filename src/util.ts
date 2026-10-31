import { createHash, randomUUID } from 'node:crypto';
import { mkdir, realpath, lstat, readFile, open, unlink } from 'node:fs/promises';
import { dirname, relative, resolve, isAbsolute, sep } from 'node:path';
export const hash = (s: string): string => createHash('sha256').update(s).digest('hex');
export function redact(value: string, secrets: string[] = []): string {
    let s = value;
    for (const secret of secrets)
        if (secret)
            s = s.split(secret).join('[REDACTED]');
    return s.replace(/(Bearer\s+)[^\s"']+/gi, '$1[REDACTED]')
        .replace(/((?:api[_-]?key|token|secret|password)\s*[=:]\s*)[^\s,;"'}]+/gi, '$1[REDACTED]');
}
export function inside(root: string, path: string): boolean {
    const rel = relative(resolve(root), resolve(path));
    return rel === '' || (rel !== '..' && !rel.startsWith(`..${sep}`) && !isAbsolute(rel));
}
export async function safeFile(root: string, path: string): Promise<string> {
    const [r, p] = await Promise.all([realpath(root), realpath(resolve(root, path))]);
    if (!inside(r, p) || !(await lstat(p)).isFile())
        throw new Error('file outside allowed workspace or not a regular file');
    return p;
}
export class AsyncQueue<T> implements AsyncIterable<T> {
    private items: T[] = [];
    private waiters: ((v: IteratorResult<T>) => void)[] = [];
    private ended = false;
    readonly limit: number;
    constructor(limit = 128) { this.limit = limit; }
    push(item: T): void { if (this.ended)
        throw new Error('input is closed'); const waiter = this.waiters.shift(); if (waiter) {
        waiter({ done: false, value: item });
        return;
    } if (this.items.length >= this.limit)
        throw new Error('input backpressure limit'); this.items.push(item); }
    end(): void { this.ended = true; for (const waiter of this.waiters.splice(0))
        waiter({ done: true, value: undefined }); }
    discard(): void { this.items = []; this.end(); }
    [Symbol.asyncIterator](): AsyncIterator<T> { return { next: () => { const item = this.items.shift(); if (item !== undefined)
            return Promise.resolve({ done: false, value: item }); if (this.ended)
            return Promise.resolve({ done: true, value: undefined }); return new Promise(resolve => this.waiters.push(resolve)); }, return: async () => { this.discard(); return { done: true, value: undefined }; } }; }
}
/** Serializes short metadata operations only, never whole agent turns. */
export class Serial {
    private tails = new Map<string, Promise<unknown>>();
    async drain(): Promise<void> { await Promise.allSettled([...this.tails.values()]); }
    run<T>(key: string, fn: () => Promise<T>): Promise<T> { const prior = this.tails.get(key) ?? Promise.resolve(); const next = prior.catch(() => { }).then(fn); this.tails.set(key, next); void next.finally(() => { if (this.tails.get(key) === next)
        this.tails.delete(key); }).catch(() => { }); return next; }
}
export async function lockDataDir(dir: string): Promise<() => Promise<void>> {
    await mkdir(dir, { recursive: true, mode: 0o700 });
    const path = resolve(dir, 'gateway.lock');
    const token = randomUUID();
    // Fail closed on stale or live locks: operators use doctor + remove a dead lock.
    let f;
    try {
        f = await open(path, 'wx', 0o600);
    }
    catch {
        throw new Error(`Data directory is locked: ${path}. Verify the recorded PID before removing a stale lock.`);
    }
    await f.writeFile(JSON.stringify({ pid: process.pid, token, startedAt: Date.now() }));
    await f.close();
    return async () => { try {
        const current = JSON.parse(await readFile(path, 'utf8'));
        if (current.token === token)
            await unlink(path);
    }
    catch { /* already removed */ } };
}
export async function ensureParent(path: string): Promise<void> { await mkdir(dirname(path), { recursive: true, mode: 0o700 }); }
export function errorText(error: unknown): string { return error instanceof Error ? error.message : String(error); }
/** Read from an already-open descriptor; on Linux re-check its actual target to close symlink races. */
export async function readBoundedFile(root: string, path: string, limit: number): Promise<Buffer> {
    const { constants } = await import('node:fs');
    const resolved = await safeFile(root, path);
    const handle = await open(resolved, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
    try {
        const st = await handle.stat();
        if (!st.isFile() || st.size > limit)
            throw new Error('file exceeds export policy');
        if (process.platform === 'linux') {
            const actual = await realpath(`/proc/self/fd/${handle.fd}`);
            if (!inside(await realpath(root), actual))
                throw new Error('file changed outside workspace');
        }
        const chunks: Buffer[] = [];
        let total = 0;
        const stream = handle.createReadStream({ autoClose: false });
        try {
            for await (const chunk of stream) {
                total += chunk.length;
                if (total > limit)
                    throw new Error('file exceeds export policy');
                chunks.push(chunk);
            }
        }
        finally {
            stream.destroy();
        }
        return Buffer.concat(chunks);
    }
    finally {
        await handle.close();
    }
}
