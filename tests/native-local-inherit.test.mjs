import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { parseConfig } from '../dist/config.js';
import { prepareWorkspace } from '../dist/workspace.js';
import { runtimeFactory } from '../dist/runtime.js';

// Node/Windows may cache homedir(), so both settings-location variants use
// one temporary user identity. It is never the real user's configuration.
const fixtureHome = await mkdtemp(join(tmpdir(), 'feishu fixture home '));
after(async () => { await rm(fixtureHome, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 }); });

for (const explicitConfigDir of [true, false]) test(`native local inheritance (${explicitConfigDir ? 'explicit config' : 'native home'}) preserves hooks, skills, MCP, plugin and YOLO question answers`, { timeout: 90000 }, async t => {
    const root = await mkdtemp(join(tmpdir(), 'feishu inherit 中文 '));
    const before = { ...process.env }, requests = [], events = [], logs = [];
    let worker, failure, target, otherAttachment, ownAttachment, hookLog;
    const project = join(root, 'existing project'), home = fixtureHome, configDir = explicitConfigDir ? join(root, 'user config') : join(home, '.claude'), marketplace = join(configDir, 'plugins', 'marketplaces', 'fixture-market'), plugin = join(marketplace, 'fixture-plugin');
    const q = value => JSON.stringify(value.replaceAll('\\', '/'));
    let calls = 0, resolveDone, rejectDone, pendingQuestion;
    const done = new Promise((resolve, reject) => { resolveDone = resolve; rejectDone = reject; });
    const questionBeforeStop = new Promise(resolve => { pendingQuestion = resolve; });
    const server = http.createServer(async (req, res) => {
        try {
            const chunks = []; for await (const chunk of req) chunks.push(chunk);
            const body = JSON.parse(Buffer.concat(chunks).toString());
            if (req.url.includes('count_tokens')) { res.writeHead(200, { 'content-type': 'application/json' }); res.end('{"input_tokens":500}'); return; }
            assert.equal(req.headers.authorization, 'Bearer fixture-only-token');
            requests.push(body); calls++;
            const tools = [
                { name: 'ToolSearch', input: { query: 'select:mcp__fixture_inherited__fixture_echo,mcp__fixture_user__fixture_echo,mcp__gateway__send_file', max_results: 5 } },
                { name: 'mcp__fixture_user__fixture_echo', input: {} },
                { name: 'Write', input: { file_path: target, content: 'DIRECT_CWD_AND_YOLO' } },
                { name: 'Read', input: { file_path: otherAttachment } },
                { name: 'Write', input: { file_path: ownAttachment, content: 'MUST_NOT_WRITE' } },
                { name: 'AskUserQuestion', input: { questions: [{ question: 'Continue fixture?', header: 'Fixture', options: [{ label: 'Yes', description: 'Continue.' }, { label: 'No', description: 'Stop.' }], multiSelect: false }] } }
            ];
            const tool = calls === 8 ? { name: 'AskUserQuestion', input: { questions: [{ question: 'Stop fixture?', header: 'Stop', options: [{ label: 'Yes', description: 'Stop.' }, { label: 'No', description: 'Continue.' }], multiSelect: false }] } } : tools[calls - 1];
            const content = tool ? { type: 'tool_use', id: 'toolu_inherit_' + calls, ...tool } : { type: 'text', text: 'INHERIT_FIXTURE_DONE' };
            const stop = tool ? 'tool_use' : 'end_turn';
            const message = { id: 'msg_inherit_' + calls, type: 'message', role: 'assistant', model: 'fixture-model', content: [content], stop_reason: stop, stop_sequence: null, usage: { input_tokens: 500, output_tokens: 30 } };
            if (!body.stream) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify(message)); return; }
            res.writeHead(200, { 'content-type': 'text/event-stream' });
            const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
            event('message_start', { message: { ...message, content: [], stop_reason: null, usage: { input_tokens: 500, output_tokens: 0 } } });
            event('content_block_start', { index: 0, content_block: tool ? { ...content, input: {} } : { type: 'text', text: '' } });
            event('content_block_delta', { index: 0, delta: tool ? { type: 'input_json_delta', partial_json: JSON.stringify(tool.input) } : { type: 'text_delta', text: content.text } });
            event('content_block_stop', { index: 0 }); event('message_delta', { delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: 30 } }); event('message_stop', {}); res.end();
        } catch (error) { failure = error; res.writeHead(500); res.end(); rejectDone(error); }
    });
    let timer;
    try {
        await Promise.all([mkdir(home, { recursive: true }), mkdir(join(project, '.claude'), { recursive: true }), mkdir(join(configDir, 'skills', 'fixture-user-skill'), { recursive: true }), mkdir(join(plugin, '.claude-plugin'), { recursive: true }), mkdir(join(plugin, 'skills', 'fixture-plugin-skill'), { recursive: true })]);
        process.env.HOME = home; process.env.USERPROFILE = home;
        if (explicitConfigDir) process.env.CLAUDE_CONFIG_DIR = configDir; else delete process.env.CLAUDE_CONFIG_DIR;
        process.env.NATIVE_FIXTURE_TOKEN = 'fixture-only-token';
        hookLog = join(root, 'hooks.log');
        const hookScript = join(root, 'hook.mjs');
        await writeFile(hookScript, "import { appendFileSync } from 'node:fs'; appendFileSync(process.argv[2], process.argv[3] + '\\n'); console.log('{}');");
        const hook = source => ({ env: { ANTHROPIC_BASE_URL: 'http://127.0.0.1:1/incorrect-settings-endpoint', ANTHROPIC_AUTH_TOKEN: 'fixture-settings-token', ANTHROPIC_MODEL: 'fixture-settings-model' }, hooks: { PreToolUse: [{ matcher: 'Write|Read|AskUserQuestion', hooks: [{ type: 'command', command: `${q(process.execPath)} ${q(hookScript)} ${q(hookLog)} ${source}` }] }] } });
        const settings = { ...hook('user'), enabledPlugins: { 'fixture-plugin@fixture-market': true }, extraKnownMarketplaces: { 'fixture-market': { source: { source: 'directory', path: marketplace } } } };
        await writeFile(join(configDir, 'settings.json'), JSON.stringify(settings));
        await writeFile(join(project, '.claude', 'settings.json'), JSON.stringify(hook('project')));
        await writeFile(join(project, '.claude', 'settings.local.json'), JSON.stringify({ ...hook('local'), enableAllProjectMcpServers: true }));
        await writeFile(join(configDir, 'skills', 'fixture-user-skill', 'SKILL.md'), '---\nname: fixture-user-skill\ndescription: FIXTURE_USER_SKILL_DESCRIPTION\n---\nFixture skill only.\n');
        await writeFile(join(plugin, '.claude-plugin', 'plugin.json'), JSON.stringify({ name: 'fixture-plugin', version: '1.0.0', description: 'Fixture plugin only.' }));
        await mkdir(join(marketplace, '.claude-plugin'), { recursive: true });
        await writeFile(join(marketplace, '.claude-plugin', 'marketplace.json'), JSON.stringify({ name: 'fixture-market', owner: { name: 'Fixture' }, plugins: [{ name: 'fixture-plugin', source: './fixture-plugin', description: 'Fixture plugin only.' }] }));
        await writeFile(join(plugin, 'skills', 'fixture-plugin-skill', 'SKILL.md'), '---\nname: fixture-plugin-skill\ndescription: FIXTURE_PLUGIN_SKILL_DESCRIPTION\n---\nFixture plugin skill only.\n');
        await writeFile(join(configDir, 'plugins', 'installed_plugins.json'), JSON.stringify({ version: 2, plugins: { 'fixture-plugin@fixture-market': [{ scope: 'user', installPath: plugin, version: '1.0.0', installedAt: new Date().toISOString(), lastUpdated: new Date().toISOString() }] } }));
        const mcpScript = join(root, 'mcp.mjs');
        await writeFile(mcpScript, `import readline from 'node:readline'; const rl=readline.createInterface({input:process.stdin}); rl.on('line', line=>{const r=JSON.parse(line); if(r.id===undefined)return; const result=r.method==='initialize'?{protocolVersion:'2024-11-05',capabilities:{tools:{}},serverInfo:{name:'fixture-inherited',version:'1'}}:r.method==='tools/list'?{tools:[{name:'fixture_echo',description:'Inherited fixture tool',inputSchema:{type:'object',properties:{}}}]}:r.method==='tools/call'?{content:[{type:'text',text:'INHERITED_MCP_RESULT'}]}:{}; console.log(JSON.stringify({jsonrpc:'2.0',id:r.id,result}));});`);
        await writeFile(join(project, '.mcp.json'), JSON.stringify({ mcpServers: { fixture_inherited: { command: process.execPath, args: [mcpScript] } } }));
        await writeFile(join(explicitConfigDir ? configDir : home, '.claude.json'), JSON.stringify({ hasCompletedOnboarding: true, mcpServers: { fixture_user: { command: process.execPath, args: [mcpScript] } } }));
        await new Promise(r => server.listen(0, '127.0.0.1', r));
        const config = parseConfig({ tenantId: 'fixture', appId: 'cli_fixture', dataDir: join(root, 'state'),
            runtime: process.env.FEISHU_NATIVE_TEST_EXECUTABLE ? { claudeExecutable: process.env.FEISHU_NATIVE_TEST_EXECUTABLE } : undefined,
            model: { baseUrl: `http://127.0.0.1:${server.address().port}`, allowHttp: true, tokenEnv: 'NATIVE_FIXTURE_TOKEN', model: 'fixture-model' }, projects: [{ id: 'p', maintainers: ['admin'], cwd: project, nativeConfig: 'inherit', permissionMode: 'bypassPermissions' }], groups: [], dms: [] });
        const room = { key: 'fixture-room', chatId: 'fixture-chat', projectId: 'p', maintainers: ['admin'] };
        const session = { id: randomUUID(), roomKey: room.key, state: 'open', createdAt: Date.now() };
        const w = await prepareWorkspace(config, room, session, config.projects[0]);
        target = join(root, 'outside-cwd.txt'); ownAttachment = join(w.inbound, 'own.txt');
        const other = await prepareWorkspace(config, { ...room, key: 'other-room' }, { ...session, id: randomUUID() }, config.projects[0]);
        otherAttachment = join(other.inbound, 'other.txt');
        await writeFile(ownAttachment, 'OWN_ATTACHMENT_UNCHANGED'); await writeFile(otherAttachment, 'OTHER_ROOM_MUST_NOT_APPEAR');
        timer = setTimeout(() => rejectDone(new Error('inherit fixture timeout: ' + JSON.stringify({ events: events.slice(-8), logs: logs.slice(-5), calls }))), 70000);
        worker = await runtimeFactory(config, ['fixture-only-token'], (event, detail) => logs.push({ event, detail }))(room, session, event => {
            events.push(event);
            if (event.type === 'error') rejectDone(new Error(event.message + JSON.stringify(logs)));
            if (event.type === 'request') {
                if (event.kind !== 'question') rejectDone(new Error('Unexpected ordinary permission prompt: ' + JSON.stringify(event)));
                else if (event.input.questions[0].question === 'Stop fixture?') pendingQuestion(event);
                else setTimeout(() => worker.respond(event.id, { approved: true, answers: { 'Continue fixture?': 'Yes' } }), 250);
            }
            if (event.type === 'result') { if (event.success) resolveDone(); else rejectDone(new Error('Native result failed: ' + JSON.stringify(event))); }
        }, w);
        await worker.send({ uuid: randomUUID(), roomKey: room.key, sessionId: session.id, message: { id: randomUUID(), chatId: room.chatId, chatType: 'group', senderId: 'admin', text: 'Execute fixture.', time: Date.now(), mentioned: true, resources: [] }, background: [], attachments: [] });
        await done;
        if (failure) throw failure;
        assert.equal(await readFile(target, 'utf8'), 'DIRECT_CWD_AND_YOLO');
        assert.equal(await readFile(ownAttachment, 'utf8'), 'OWN_ATTACHMENT_UNCHANGED');
        assert.equal(events.filter(e => e.type === 'request' && e.kind === 'question').length, 1);
        const last = JSON.stringify(requests.at(-1));
        assert.ok(last.includes('"Continue fixture?"=\\"Yes\\"') || last.includes('Continue fixture?\\"=\\"Yes'), 'actual native question result must retain the human answer: ' + last.slice(-1800));
        assert.ok(!last.includes('OTHER_ROOM_MUST_NOT_APPEAR'));
        assert.ok(last.includes('INHERITED_MCP_RESULT'), 'user global .claude.json MCP must execute, including when the config override is unset');
        const observedHooks = (await readFile(hookLog, 'utf8')).trim().split('\n');
        for (const source of ['user', 'project', 'local']) assert.ok(observedHooks.includes(source), source + ' hook must run alongside SDK hooks');
        const first = JSON.stringify(requests[0]);
        assert.ok(first.includes('fixture-user-skill'), 'user skill must be discovered');
        assert.ok(first.includes('fixture-plugin-skill'), 'enabled installed plugin skill must be discovered');
        assert.ok(requests.some(request => request.tools.some(tool => tool.name === 'mcp__fixture_inherited__fixture_echo')), 'project MCP must merge with gateway MCP: ' + JSON.stringify(requests.map(request => request.tools.map(tool => tool.name))));
        assert.ok(requests.some(request => request.tools.some(tool => tool.name === 'mcp__gateway__send_file')));
        assert.equal(await readFile(join(configDir, 'settings.json'), 'utf8'), JSON.stringify(settings), 'source user settings must remain unchanged');
        assert.equal(await readFile(join(project, '.claude', 'settings.json'), 'utf8'), JSON.stringify(hook('project')));
        await worker.send({ uuid: randomUUID(), roomKey: room.key, sessionId: session.id, message: { id: randomUUID(), chatId: room.chatId, chatType: 'group', senderId: 'admin', text: 'Ask stop fixture.', time: Date.now(), mentioned: true, resources: [] }, background: [], attachments: [] });
        const waiting = await Promise.race([questionBeforeStop, new Promise((_, reject) => setTimeout(() => reject(new Error('stop question was not requested')), 10000).unref())]);
        await worker.stop();
        const callsAtStop = calls;
        worker.respond(waiting.id, { approved: true, answers: { 'Stop fixture?': 'Yes' } });
        assert.equal(calls, callsAtStop, 'late answers must not be delivered after confirmed process stop');
        assert.ok(!JSON.stringify(requests.at(-1)).includes('Stop fixture?\\"=\\"Yes'), 'stop must not invent a human answer');
        assert.equal(events.filter(e => e.type === 'result').length, 1, 'stop must not report the pending question as completed');
        assert.ok(!(await readdir(w.root)).some(name => name.startsWith('native-settings-')), 'invocation model overlay must be removed on normal stop');
        t.diagnostic(`Actual ${process.env.FEISHU_NATIVE_TEST_EXECUTABLE ? 'explicitly configured' : 'SDK bundled'} CC with pinned SDK and local deterministic inference, temporary user settings/plugin/MCP; no real Feishu or external model.`);
    } finally {
        clearTimeout(timer); await worker?.stop(); server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
        process.env = before;
        await rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
    }
});
