import http from 'node:http';
import { randomBytes, timingSafeEqual } from 'node:crypto';
import { unlink, chmod } from 'node:fs/promises';
async function drained(stream: http.ServerResponse): Promise<void> {
    if (stream.destroyed)
        throw new Error('client closed');
    await new Promise<void>((resolve, reject) => {
        const cleanup = () => { stream.off('drain', onDrain); stream.off('close', onClose); stream.off('error', onError); };
        const onDrain = () => { cleanup(); resolve(); };
        const onClose = () => { cleanup(); reject(new Error('client closed')); };
        const onError = (error: Error) => { cleanup(); reject(error); };
        stream.once('drain', onDrain);
        stream.once('close', onClose);
        stream.once('error', onError);
    });
}
import type { Config } from './types.ts';
export class ModelBudget {
    active = 0;
    readonly max: number;
    constructor(max: number) { this.max = max; }
}
export interface ModelEndpoint {
    url?: string;
    socket?: string;
    token: string;
    close: () => Promise<void>;
}
function equal(a: string, b: string): boolean { const x = Buffer.from(a), y = Buffer.from(b); return x.length === y.length && timingSafeEqual(x, y); }
/** The worker can reach only this fixed upstream, not an arbitrary HTTP proxy. */
export async function startModelProxy(config: Config, budget: ModelBudget, socket?: string): Promise<ModelEndpoint> {
    const token = randomBytes(32).toString('hex');
    const secret = process.env[config.model.tokenEnv];
    if (!secret)
        throw new Error(`missing model credential ${config.model.tokenEnv}`);
    const upstreamBase = config.model.baseUrl.replace(/\/$/, '');
    const controllers = new Set<AbortController>();
    const server = http.createServer(async (req, res) => {
        const reject = (status: number, message: string) => { res.writeHead(status, { 'content-type': 'application/json' }); res.end(JSON.stringify({ type: 'error', error: { type: status === 429 ? 'rate_limit_error' : 'invalid_request_error', message } })); };
        const credential = String(req.headers.authorization ?? '').replace(/^Bearer /, '') || String(req.headers['x-api-key'] ?? '');
        if (!equal(credential, token)) {
            reject(401, 'invalid worker capability');
            return;
        }
        let path: string;
        try {
            path = new URL(req.url ?? '/', 'http://local').pathname;
        }
        catch {
            reject(400, 'invalid path');
            return;
        }
        if (req.method !== 'POST' || !['/v1/messages', '/v1/messages/count_tokens'].includes(path)) {
            reject(403, 'endpoint not allowed');
            return;
        }
        if (budget.active >= budget.max) {
            res.setHeader('retry-after', '2');
            reject(429, 'shared model concurrency limit');
            return;
        }
        budget.active++;
        const controller = new AbortController();
        controllers.add(controller);
        const timeout = setTimeout(() => controller.abort(), 30 * 60 * 1000);
        timeout.unref();
        const disconnected = () => { if (!res.writableFinished)
            controller.abort(); };
        res.once('close', disconnected);
        req.once('aborted', () => controller.abort());
        try {
            const chunks: Buffer[] = [];
            let size = 0;
            for await (const chunk of req) {
                const b = Buffer.from(chunk);
                size += b.length;
                if (size > 32 * 1024 * 1024) {
                    reject(413, 'request too large');
                    return;
                }
                chunks.push(b);
            }
            const body = Buffer.concat(chunks);
            let json: Record<string, unknown>;
            try {
                json = JSON.parse(body.toString('utf8'));
            }
            catch {
                reject(400, 'invalid JSON');
                return;
            }
            if (!json || ![config.model.model, config.model.smallModel].includes(String(json.model))) {
                reject(403, 'model not allowed');
                return;
            }
            const headers: Record<string, string> = { 'content-type': 'application/json', 'authorization': `Bearer ${secret}`, 'anthropic-version': '2023-06-01' };
            if (typeof req.headers['anthropic-beta'] === 'string')
                headers['anthropic-beta'] = req.headers['anthropic-beta'].slice(0, 1024);
            for (const [k, v] of Object.entries(config.model.extraHeaders))
                headers[k] = v.startsWith('env:') ? (process.env[v.slice(4)] ?? '') : v;
            const tail = upstreamBase.endsWith('/v1') ? path.slice(3) : path;
            const response = await fetch(upstreamBase + tail + (req.url?.includes('?') ? '?' + req.url.split('?')[1] : ''), { method: 'POST', body, headers, signal: controller.signal, redirect: 'manual' });
            if (!response.ok) {
                await response.body?.cancel();
                reject(response.status >= 300 && response.status < 400 ? 502 : response.status, `company gateway returned HTTP ${response.status}`);
                return;
            }
            res.writeHead(response.status, { 'content-type': response.headers.get('content-type') ?? 'application/json', 'cache-control': 'no-store' });
            if (response.body)
                for await (const data of response.body) {
                    if (!res.write(Buffer.from(data)))
                        await drained(res);
                }
            res.end();
        }
        catch {
            if (!res.headersSent)
                reject(502, 'company gateway transport failed');
            else
                res.destroy();
        }
        finally {
            clearTimeout(timeout);
            controllers.delete(controller);
            budget.active--;
            res.off('close', disconnected);
        }
    });
    server.headersTimeout = 15000;
    server.requestTimeout = 30000;
    if (socket) {
        try {
            await unlink(socket);
        }
        catch { /* new socket */ }
    }
    await new Promise<void>((resolve, reject) => { server.once('error', reject); if (socket)
        server.listen(socket, resolve);
    else
        server.listen(0, '127.0.0.1', resolve); });
    if (socket)
        await chmod(socket, 0o600);
    const address = server.address();
    return { token, socket, url: typeof address === 'object' && address ? `http://127.0.0.1:${address.port}` : undefined,
        close: async () => { for (const c of controllers)
            c.abort(); server.closeAllConnections(); await new Promise<void>(resolve => server.close(() => resolve())); if (socket)
            try {
                await unlink(socket);
            }
            catch { /* already gone */ } } };
}
/** Linux workers have --network=none; a read-only mounted UNIX socket carries model traffic. */
export async function localSocketRelay(socket: string): Promise<{
    url: string;
    close: () => void;
}> {
    const server = http.createServer((req, res) => {
        const forward = http.request({ socketPath: socket, path: req.url, method: req.method, headers: req.headers }, upstream => { res.writeHead(upstream.statusCode ?? 502, upstream.headers); upstream.pipe(res); });
        forward.on('error', () => { if (!res.headersSent)
            res.writeHead(502); res.end(); });
        req.once('aborted', () => forward.destroy());
        res.once('close', () => forward.destroy());
        req.pipe(forward);
    });
    await new Promise<void>((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
    const address = server.address();
    if (!address || typeof address === 'string')
        throw new Error('relay failed');
    return { url: `http://127.0.0.1:${address.port}`, close: () => { server.closeAllConnections(); server.close(); } };
}
