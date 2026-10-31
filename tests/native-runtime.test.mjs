import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { parseConfig } from '../dist/config.js';
import { prepareWorkspace } from '../dist/workspace.js';
import { runtimeFactory } from '../dist/runtime.js';

// Real native SDK/CLI and production launcher; only inference is a deterministic fixture.
for (const permissionMode of ['default', 'acceptEdits']) {
    test(`native service: direct model endpoint, warm reuse, ${permissionMode}`, { timeout: 60000 }, async t => {
        const root = await mkdtemp(join(tmpdir(), 'feishu native 中文 '));
        const requests = [], timings = [], logs = [];
        let calls = 0, failure, worker, permissionCount = 0, resultCount = 0, target, attachment;
        const events = [];
        const savedToken = process.env.NATIVE_FIXTURE_TOKEN;
        const savedTimezone = process.env.TZ;
        const timezone = permissionMode === 'default' ? 'UTC' : 'Europe/Paris';
        process.env.TZ = timezone;
        process.env.NATIVE_FIXTURE_TOKEN = 'fixture-model-token';
        const server = http.createServer(async (req, res) => {
            const chunks = []; for await (const chunk of req) chunks.push(chunk);
            try {
                assert.ok(['/api-prefix/v1/messages', '/api-prefix/v1/messages/count_tokens'].includes(new URL(req.url, 'http://local').pathname));
                assert.equal(req.headers.authorization, 'Bearer fixture-model-token');
                assert.equal(req.headers['x-test-project'], 'fixture-project');
                const body = JSON.parse(Buffer.concat(chunks).toString());
                if (req.url.includes('count_tokens')) { res.writeHead(200, { 'content-type': 'application/json' }); res.end('{"input_tokens":500}'); return; }
                requests.push(body); calls++;
                const content = calls === 1 ? { type: 'tool_use', id: 'toolu_write', name: 'Write', input: { file_path: target, content: 'native-edited-content' } }
                    : calls === 3 ? { type: 'tool_use', id: 'toolu_read', name: 'Read', input: { file_path: attachment } }
                    : { type: 'text', text: 'Native fixture completed.' };
                const stop = content.type === 'tool_use' ? 'tool_use' : 'end_turn';
                const message = { id: 'msg_' + calls, type: 'message', role: 'assistant', model: 'fixture-model', content: [content], stop_reason: stop, stop_sequence: null, usage: { input_tokens: 500, output_tokens: 30 } };
                if (!body.stream) { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify(message)); return; }
                res.writeHead(200, { 'content-type': 'text/event-stream' });
                const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
                event('message_start', { message: { ...message, content: [], stop_reason: null, usage: { input_tokens: 500, output_tokens: 0 } } });
                event('content_block_start', { index: 0, content_block: content.type === 'text' ? { type: 'text', text: '' } : { ...content, input: {} } });
                event('content_block_delta', { index: 0, delta: content.type === 'text' ? { type: 'text_delta', text: content.text } : { type: 'input_json_delta', partial_json: JSON.stringify(content.input) } });
                event('content_block_stop', { index: 0 }); event('message_delta', { delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: 30 } }); event('message_stop', {}); res.end();
            } catch (error) { failure = error; res.writeHead(500); res.end(); }
        });
        await new Promise(r => server.listen(0, '127.0.0.1', r));
        const config = parseConfig({ tenantId: 'fixture', appId: 'cli_fixture', dataDir: root,
            model: { baseUrl: `http://127.0.0.1:${server.address().port}/api-prefix${permissionMode === 'acceptEdits' ? '/v1' : ''}`, allowHttp: true, tokenEnv: 'NATIVE_FIXTURE_TOKEN', model: 'fixture-model', extraHeaders: { 'x-test-project': 'fixture-project' } },
            projects: [{ id: 'p', maintainers: ['admin'], permissionMode }], groups: [], dms: [] });
        const room = { key: 'room', chatId: 'chat', projectId: 'p', maintainers: ['admin'] };
        const session = { id: randomUUID(), roomKey: room.key, state: 'open', createdAt: Date.now() };
        const w = await prepareWorkspace(config, room, session, config.projects[0]);
        target = join(w.work, '审批 文件.txt'); attachment = join(w.inbound, '外部 附件.txt');
        await writeFile(attachment, 'SCOPED_ATTACHMENT_CONTENT');
        const delivery = (text, sender = 'alice') => ({ uuid: randomUUID(), roomKey: room.key, sessionId: session.id,
            message: { id: randomUUID(), chatId: room.chatId, chatType: 'group', senderId: sender, text, time: Date.now(), mentioned: true, resources: [] }, background: [], attachments: [] });
        const first = delivery('FIRST_NATIVE_REQUEST'), second = delivery('BUSY_CORRECTION_FROM_BOB', 'bob'), third = delivery('THIRD_TURN_CONTINUATION');
        let resolveDone, rejectDone;
        const done = new Promise((resolve, reject) => { resolveDone = resolve; rejectDone = reject; });
        let sentAt = performance.now();
        const timer = setTimeout(() => rejectDone(new Error('native fixture timeout\n' + JSON.stringify(logs))), 50000);
        try {
            worker = await runtimeFactory(config, ['fixture-model-token'], (name, value) => logs.push({ name, value }))(room, session, event => {
                events.push(event);
                try {
                    if (event.type === 'error') throw new Error(event.message + '\n' + JSON.stringify(logs));
                    if (event.type === 'request') {
                        permissionCount++; assert.equal(event.kind, 'permission'); assert.equal(event.name, 'Write');
                        void worker.send(second).then(() => worker.respond(event.id, { approved: true })).catch(rejectDone);
                    }
                    if (event.type === 'result') {
                        assert.equal(event.success, true); resultCount++;
                        timings.push(Math.round(performance.now() - sentAt));
                        if (resultCount === 1) {
                            assert.ok(event.uuids.includes(first.uuid));
                            if (permissionMode === 'default') assert.ok(event.uuids.includes(second.uuid), 'busy follow-up belongs to this result');
                            sentAt = performance.now(); void worker.send(third).catch(rejectDone);
                        } else resolveDone();
                    }
                } catch (error) { rejectDone(error); }
            }, w);
            await worker.send(first); await done;
            if (failure) throw failure;
            assert.equal(permissionCount, permissionMode === 'default' ? 1 : 0);
            assert.equal(resultCount, 2);
            assert.equal(new Set(events.filter(e => e.type === 'ready').map(e => e.ccId)).size, 1);
            assert.equal(logs.filter(e => e.name === 'worker_ready').length, 1, 'warm turn must not start a new process');
            assert.equal(await readFile(target, 'utf8'), 'native-edited-content');
            if (permissionMode === 'default') assert.ok(JSON.stringify(requests[1]).includes('BUSY_CORRECTION_FROM_BOB'));
            assert.ok(JSON.stringify(requests.at(-1)).includes('FIRST_NATIVE_REQUEST'));
            assert.ok(JSON.stringify(requests.at(-1)).includes('SCOPED_ATTACHMENT_CONTENT'));
            const userText = requests[0].messages.filter(m => m.role === 'user').flatMap(m => typeof m.content === 'string'
                ? [m.content] : m.content.filter(b => b.type === 'text').map(b => b.text)).join('\n');
            assert.ok(userText.includes(`"timezone":"${timezone}"`), 'input timezone follows the native process environment');
            assert.ok(JSON.stringify(requests[0].system).includes('language of the latest human request'));
            for (const tool of ['Agent', 'Skill', 'ScheduleWakeup']) assert.ok(requests[0].tools.some(t => t.name === tool));
            assert.ok(!requests[0].tools.some(t => t.name.endsWith('project_action')), 'no empty action catalogue');
            assert.equal(events.some(e => e.type === 'thinking'), false);
            t.diagnostic(`Local deterministic model only: first work ${timings[0]} ms, warm continuation ${timings[1]} ms; one native session.`);
        } finally {
            clearTimeout(timer); await worker?.stop(); server.closeAllConnections(); await new Promise(r => server.close(r));
            await rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
            if (savedToken === undefined) delete process.env.NATIVE_FIXTURE_TOKEN; else process.env.NATIVE_FIXTURE_TOKEN = savedToken;
            if (savedTimezone === undefined) delete process.env.TZ; else process.env.TZ = savedTimezone;
        }
    });
}
