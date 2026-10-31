import http from 'node:http';
import { randomBytes, timingSafeEqual } from 'node:crypto';
import type { Args } from './types.ts';
/** Native Monitor may call the same checked host operations as the MCP tool.
 * It never receives a service credential, selectable URL, or cross-room route.
 */
export async function startActionRelay(names: string[], invoke: (name: string, input: Args) => Promise<Args>): Promise<{
    url: string;
    token: string;
    close: () => void;
}> {
    const token = randomBytes(32).toString('hex');
    let active = 0;
    const server = http.createServer(async (req, res) => {
        const fail = (code: number) => { res.writeHead(code); res.end(); };
        const supplied = String(req.headers.authorization ?? '').replace(/^Bearer /, '');
        if (Buffer.byteLength(supplied) !== Buffer.byteLength(token) || !timingSafeEqual(Buffer.from(supplied), Buffer.from(token))) {
            fail(401);
            return;
        }
        if (req.method !== 'POST' || req.url !== '/' || active >= 8) {
            fail(429);
            return;
        }
        active++;
        try {
            const chunks: Buffer[] = [];
            let size = 0;
            for await (const chunk of req) {
                size += chunk.length;
                if (size > 65536) {
                    fail(413);
                    return;
                }
                chunks.push(Buffer.from(chunk));
            }
            const data = JSON.parse(Buffer.concat(chunks).toString()) as Args;
            if (!names.includes(String(data.action)) || !data.parameters || typeof data.parameters !== 'object' || Array.isArray(data.parameters)) {
                fail(400);
                return;
            }
            const result = await invoke(String(data.action), data.parameters as Args);
            res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-store' });
            res.end(JSON.stringify(result));
        }
        catch {
            if (!res.headersSent)
                fail(502);
            else
                res.destroy();
        }
        finally {
            active--;
        }
    });
    server.requestTimeout = 15000;
    server.headersTimeout = 10000;
    await new Promise<void>((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
    const address = server.address();
    if (!address || typeof address === 'string')
        throw new Error('Action relay failed');
    return { url: `http://127.0.0.1:${address.port}/`, token, close: () => { server.closeAllConnections(); server.close(); } };
}
