export type ProbeState = 'passed' | 'failed' | 'blocked' | 'not_checked';
export interface ProbeCheck { name: string; ok: boolean; detail: string; skipped?: boolean; }
export interface IdentityEvidence {
    auth: ProbeState;
    identity: ProbeState;
    checks: ProbeCheck[];
}

const origin = 'https://open.feishu.cn';
class ProbeFailure extends Error {}

async function request(path: string, init: RequestInit, signal: AbortSignal): Promise<Record<string, unknown>> {
    let response: Response;
    try { response = await fetch(origin + path, { ...init, redirect: 'manual', signal }); }
    catch { throw new ProbeFailure(signal.aborted ? 'Request deadline exceeded; check connectivity.' : 'Network request failed; check DNS, proxy and CA configuration.'); }
    if (!response.ok) {
        await response.body?.cancel().catch(() => {});
        throw new ProbeFailure(`HTTP ${response.status}; check application access and network configuration. Response body is not displayed.`);
    }
    const reader = response.body?.getReader();
    const chunks: Uint8Array[] = [];
    let bytes = 0;
    try {
        if (reader) for (;;) {
            const { value, done } = await reader.read();
            if (done) break;
            bytes += value.byteLength;
            if (bytes > 65536) throw new ProbeFailure('Response exceeded 64 KiB; identity was not verified.');
            chunks.push(value);
        }
    } catch (error) {
        if (error instanceof ProbeFailure) throw error;
        throw new ProbeFailure(signal.aborted ? 'Response deadline exceeded.' : 'Response could not be read.');
    } finally { await reader?.cancel().catch(() => {}); }
    let data: unknown;
    try { data = JSON.parse(Buffer.concat(chunks).toString('utf8')); }
    catch { throw new ProbeFailure('Expected a JSON API response, not a login or proxy page.'); }
    if (!data || typeof data !== 'object' || Array.isArray(data)) throw new ProbeFailure('Invalid API response shape.');
    const object = data as Record<string, unknown>;
    if (object.code !== 0) {
        const code = typeof object.code === 'number' && Number.isSafeInteger(object.code) ? String(object.code) : 'missing/invalid';
        throw new ProbeFailure(`Feishu API code ${code}. Check application configuration; provider error text is not displayed.`);
    }
    return object;
}

/** Read identity only. Never connect the event source, send messages or alter permissions. */
export async function probeFeishuIdentity(appId: string, appSecret: string): Promise<IdentityEvidence> {
    const signal = AbortSignal.timeout(15000);
    const result: IdentityEvidence = { auth: 'failed', identity: 'blocked', checks: [] };
    let token: string;
    try {
        const data = await request('/open-apis/auth/v3/tenant_access_token/internal', {
            method: 'POST', headers: { 'content-type': 'application/json' },
            body: JSON.stringify({ app_id: appId, app_secret: appSecret })
        }, signal);
        if (typeof data.tenant_access_token !== 'string' || !data.tenant_access_token.trim())
            throw new ProbeFailure('Successful response did not contain an application access token.');
        token = data.tenant_access_token;
        result.auth = 'passed';
        result.checks.push({ name: 'Feishu application authentication', ok: true, detail: 'Application token exchange succeeded. Token is not displayed or saved.' });
    } catch (error) {
        result.checks.push({ name: 'Feishu application authentication', ok: false,
            detail: error instanceof ProbeFailure ? error.message : 'Application authentication failed.' });
        result.checks.push({ name: 'Feishu bot identity', ok: false, skipped: true, detail: 'Not checked: application authentication must succeed first.' });
        return result;
    }
    try {
        const data = await request('/open-apis/bot/v3/info', { method: 'GET', headers: { authorization: `Bearer ${token}` } }, signal);
        const bot = data.bot as Record<string, unknown> | undefined;
        if (!bot || typeof bot.open_id !== 'string' || !bot.open_id.trim())
            throw new ProbeFailure('Successful response did not contain a bot identity. Check that the application has a bot.');
        result.identity = 'passed';
        result.checks.push({ name: 'Feishu bot identity', ok: true,
            detail: 'Bot identity returned. Event delivery, card callbacks, file access and application publication were not tested.' });
    } catch (error) {
        result.identity = 'failed';
        result.checks.push({ name: 'Feishu bot identity', ok: false,
            detail: error instanceof ProbeFailure ? error.message : 'Bot identity lookup failed.' });
    }
    return result;
}
