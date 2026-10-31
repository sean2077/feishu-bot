#!/usr/bin/env node
/** A room-scoped transport for native Bash/Monitor commands, not a scheduler. */
const [action, parameters = '{}'] = process.argv.slice(2);
async function main(): Promise<void> {
    if (!action)
        throw new Error('Usage: node "$FEISHU_ACTION_CLIENT" operation \'{"parameter":"value"}\'');
    const url = process.env.FEISHU_ACTION_URL, token = process.env.FEISHU_ACTION_TOKEN;
    if (!url || !token)
        throw new Error('This command must run inside a feishu-bot worker');
    const response = await fetch(url, { method: 'POST', headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` }, body: JSON.stringify({ action, parameters: JSON.parse(parameters) }), signal: AbortSignal.timeout(15 * 60 * 1000), redirect: 'error' });
    const text = await response.text();
    if (!response.ok)
        throw new Error(`Action bridge returned HTTP ${response.status}`);
    const result = JSON.parse(text);
    console.log(JSON.stringify(result));
    if (!result.ok)
        process.exitCode = 1;
}
main().catch(error => { console.error(error instanceof Error ? error.message : 'Action failed'); process.exitCode = 1; });
