import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { migrationFixture, until, deferred, envelopes, fields, sumUsage } from './migration-fixture.mjs';

const tokenView = value => Object.fromEntries(fields.map(field => [field, value[field]]));
const resultFor = (f, uuid) => f.events.find(e => e.type === 'result' && (e.uuid === uuid || e.uuids?.includes(uuid)));
const uuidFor = (f, id) => f.store.db.prepare('SELECT uuid FROM inbox WHERE id=?').get(id)?.uuid;

test('actual streaming CC usage is per result, reports response models and survives native resume without cumulative double counting', { timeout: 90000 }, async t => {
    const f = await migrationFixture((_f, request) => ({ text: `USAGE_REPLY_${request.number}`, model: `fixture-reported-${request.mode}` }));
    try {
        assert.equal((await f.usage()).session, null);
        for (const mode of ['first', 'second']) {
            f.mode = mode;
            const start = f.requests.length, id = await f.text(`USAGE_${mode}`);
            await until(() => f.done(id), `native ${mode} completion`);
            assert.equal(f.requests.length - start, 1, 'one deterministic text request in this turn');
            const event = resultFor(f, uuidFor(f, id));
            assert.ok(event?.resultId, 'SDK result UUID must be forwarded, never fabricated from input ID');
            assert.notEqual(event.resultId, uuidFor(f, id));
            assert.deepEqual(tokenView(event.usage), sumUsage(f.requests.slice(start)), 'result usage must not include previous streaming turns');
            assert.deepEqual(event.models, [`fixture-reported-${mode}`]);
            const report = await f.usage();
            assert.deepEqual(report.latest.usage, tokenView(event.usage));
            assert.deepEqual(report.latest.models, event.models);
            assert.deepEqual(report.cumulative.usage, sumUsage(f.requests));
            assert.equal(report.historyIncomplete, false);
            assert.equal(report.unidentifiedResults, false);
            assert.equal(report.cost, null); assert.equal(report.contextOccupancy, null);
        }
        assert.equal(f.workers.length, 1, 'warm turns must use one actual native process');
        const selected = { ...f.selected() }, before = await f.usage();
        await f.restart();
        assert.deepEqual({ ...f.selected() }, selected);
        assert.deepEqual(await f.usage(), before, 'reopening SQLite must not backfill or count a result again');
        f.mode = 'resumed';
        const start = f.requests.length, id = await f.text('USAGE_after_restart');
        await until(() => f.done(id), 'resumed native completion');
        assert.equal(f.workers.length, 2); assert.equal(f.workers[1].resumed, selected.cc);
        const event = resultFor(f, uuidFor(f, id));
        assert.deepEqual(tokenView(event.usage), sumUsage(f.requests.slice(start)), 'resumed result excludes old transcript usage');
        const report = await f.usage();
        assert.equal(report.cumulative.results, 3);
        assert.deepEqual(report.cumulative.usage, sumUsage(f.requests));
        assert.equal(new Set(f.events.filter(e => e.type === 'result').map(e => e.resultId)).size, 3);
        assert.deepEqual(f.errors, []);
        t.diagnostic('Pinned real CC/SDK, synthetic model usage: three distinct native results; per-turn counts match endpoint declarations before and after resume. No billing claim.');
    } finally { await f.close(); }
});

test('production raw entry: attributed supplements, approval, compact barrier, complete delivery, stop/resume and durable image restart', { timeout: 120000 }, async t => {
    const firstGate = deferred(), fileGate = deferred();
    const longAnswer = 'JOURNEY_FINAL\n' + 'Complete fixture result.\n'.repeat(1200);
    let issuedAction = false, issuedReads = false;
    const f = await migrationFixture(async (f, request) => {
        if (request.mode === 'task') {
            if (!issuedAction) {
                issuedAction = true;
                await firstGate.promise;
                return { content: [{ type: 'tool_use', id: 'tool_fixture_operation', name: 'mcp__gateway__project_action', input: { action: 'record_once', parameters: {} } }] };
            }
            const raw = JSON.stringify(request.body.messages);
            assert.ok(raw.includes('BODY_BOB') && raw.includes('BODY_ALICE'), 'both authors must reach the actual model request');
            assert.ok(raw.includes('ACTION_OK'), 'the native tool must finish the approved operation');
            assert.ok(!raw.includes('LATE_SUPPLEMENT'), 'queued compact boundary must not be crossed');
            return { content: [{ type: 'thinking', thinking: 'JOURNEY_VISIBLE_THINKING' }, { type: 'text', text: longAnswer }],
                afterBlock: index => index === 0 ? new Promise(r => setTimeout(r, 350)) : undefined };
        }
        if (request.mode === 'compact') return { text: 'JOURNEY_MEMORY: operation executed once; preserve author attribution, pending file results and this conversation.' };
        if (request.mode === 'resume') {
            assert.ok(JSON.stringify(request.body.messages).includes('JOURNEY_MEMORY'), 'native resume must retain compacted context');
            return { text: 'JOURNEY_RESUMED' };
        }
        if (request.mode === 'images') {
            if (!issuedReads) {
                issuedReads = true;
                const batch = envelopes(request.body).findLast(value => value.imageBatch)?.imageBatch;
                assert.equal(batch?.length, 2, 'one actual native input retains both original image envelopes');
                assert.equal(new Set(batch.map(d => d.uuid)).size, 2);
                assert.deepEqual(batch.map(d => d.trigger.senderId), ['bob', 'bob']);
                assert.equal(new Set(batch.map(d => d.trigger.id)).size, 2);
                return { content: batch.map((d, index) => ({ type: 'tool_use', id: `tool_fixture_image_${index}`, name: 'Read', input: { file_path: d.attachments[0].path } })) };
            }
            const results = request.body.messages.flatMap(m => Array.isArray(m.content) ? m.content : []).filter(b => b.type === 'tool_result' && /^tool_fixture_image_/.test(b.tool_use_id));
            assert.equal(results.length, 2, 'both images are read through actual native file tools');
            assert.ok(results.every(result => !result.is_error), 'native image reads must succeed');
            return { text: 'JOURNEY_IMAGES_COMPLETE' };
        }
        throw new Error(`unexpected fixture phase ${request.mode}`);
    }, { actions: true });
    f.mode = 'task';
    f.onEvent = event => { if (event.type === 'compaction' && event.phase === 'start') f.mode = 'compact'; };
    f.onFile = () => fileGate.promise;
    try {
        const unauthorized = f.raw('text', { text: '@_user_1 UNAUTHORIZED_INPUT' }, { author: 'stranger' });
        await f.receive(unauthorized);
        assert.equal(f.workers.length, 0); assert.equal(f.sent.length, 0); assert.equal(f.receipts.length, 0);
        const first = await f.text('JOURNEY_START');
        await until(() => f.requests.length === 1, 'first native request');
        const selected = { ...f.selected() };
        assert.ok(selected.cc);
        f.quotes.set('om_fixture_quote', { id: 'om_fixture_quote', chatId: 'group1', chatType: 'group', senderId: 'bob',
            text: 'AUTHORIZED_QUOTE', time: Date.now(), mentioned: false, resources: [] });
        const bob = await f.text('/btw /new BODY_BOB', { author: 'bob', reply: 'om_fixture_quote', thread: 'omt_other' });
        const alice = await f.text('/ps /compact BODY_ALICE', { author: 'alice' });
        await until(() => f.events.filter(e => e.type === 'accepted').length === 3, 'supplements enter the actual native stream');
        assert.equal(f.workers.length, 1); assert.equal(f.workers[0].inputs.length, 3);
        assert.deepEqual(f.workers[0].inputs.map(d => d.message.senderId), ['alice', 'bob', 'alice']);
        assert.equal(f.workers[0].inputs[1].message.text, '/new BODY_BOB');
        assert.equal(f.workers[0].inputs[2].message.text, '/compact BODY_ALICE');
        assert.equal(f.workers[0].inputs[1].quote?.text, 'AUTHORIZED_QUOTE');
        assert.equal(f.events.filter(e => e.type === 'compaction').length, 0, 'slash bodies are literal supplementation');
        firstGate.resolve();
        await until(() => f.sent.some(s => s.card?.header?.title?.content === '需要维护者批准'), 'actual operation approval');
        const approval = f.sent.find(s => s.card?.header?.title?.content === '需要维护者批准');
        const allow = approval.card.body.elements.find(element => element.value?.decision === 'allow').value;
        const compact = await f.text('/compact preserve JOURNEY_MEMORY', { author: 'admin' });
        const late = await f.text('/btw LATE_SUPPLEMENT', { author: 'bob' });
        assert.equal(f.status(compact), 'queued'); assert.equal(f.status(late), 'queued');
        assert.equal(f.events.filter(e => e.type === 'compaction').length, 0);
        const denied = await f.click(approval, 'bob', allow);
        assert.match(denied.toast.content, /仅维护者/);
        assert.equal(await readFile(f.marker, 'utf8').catch(() => ''), '');
        const approved = await f.click(approval, 'admin', allow);
        assert.match(approved.toast.content, /已批准一次/);
        await f.click(approval, 'admin', allow); // Platform duplicate must not execute twice.
        await until(() => f.files.length === 1, 'required full-answer file delivery started');
        assert.equal(f.files[0].done, false);
        for (const id of [first, bob, alice]) assert.equal(f.done(id), false, 'no Done before required file receipt');
        assert.equal(f.status(late), 'cancelled', 'lost foreground target cannot become a task after compact');
        assert.equal(f.status(compact), 'queued', 'native compact waits for active final delivery');
        assert.equal(await readFile(f.marker, 'utf8'), 'executed\n');
        const ids = [first, bob, alice].map(id => uuidFor(f, id));
        const terminal = resultFor(f, ids[0]);
        assert.deepEqual(new Set(terminal.uuids), new Set(ids), 'native result attributes all consumed authors');
        assert.deepEqual(tokenView(terminal.usage), sumUsage(f.requests.filter(r => r.mode === 'task')));
        fileGate.resolve();
        await until(() => [first, bob, alice, compact].every(id => f.done(id)), 'all required delivery and native compact confirmed');
        assert.equal(f.files[0].bytes.toString(), longAnswer);
        assert.equal(f.status(late), 'cancelled'); assert.equal(f.done(late), false);
        assert.equal(resultFor(f, uuidFor(f, compact)).compact, 'compacted');
        assert.deepEqual({ ...f.selected() }, selected);
        const replies = f.sent.filter(s => s.card && s.replyTo === first);
        assert.equal(replies.length, 1, 'supplements share the task answer card');
        assert.equal(f.sent.filter(s => s.card && [bob, alice].includes(s.replyTo)).length, 0);
        const rendered = JSON.stringify([replies[0].card, ...f.updates.filter(u => u.id === replies[0].id).map(u => u.card)]);
        assert.ok(rendered.includes('JOURNEY_VISIBLE_THINKING') && rendered.includes('JOURNEY_FINAL'));
        assert.ok(!rendered.includes('fixture-signature-not-display-content') && !rendered.includes('record_once'));
        assert.equal(f.requests.filter(r => r.mode === 'task').length, 2, 'operation plus final answer, not an extra supplement loop');
        assert.ok(f.requests.every(r => !JSON.stringify(r.body).includes('LATE_SUPPLEMENT')));
        await f.text('/stop', { author: 'admin' });
        assert.deepEqual({ ...f.selected() }, selected);
        const calls = f.requests.length;
        await f.text('/ps not a new task');
        assert.equal(f.requests.length, calls); assert.equal(f.workers.length, 1);
        f.mode = 'resume';
        const resumed = await f.text('JOURNEY_CONTINUE');
        await until(() => f.done(resumed), 'native resume after stop');
        assert.equal(f.workers.length, 2); assert.equal(f.workers[1].resumed, selected.cc);
        assert.deepEqual({ ...f.selected() }, selected);
        f.config.limits.imageBatchMs = 5000;
        const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aT1cAAAAASUVORK5CYII=', 'base64');
        f.resources.set('img_journey_one', png); f.resources.set('img_journey_two', png);
        const image1 = await f.receive(f.raw('image', { image_key: 'img_journey_one' }, { author: 'bob', thread: 'omt_images' }));
        const image2 = await f.receive(f.raw('image', { image_key: 'img_journey_two' }, { author: 'bob', thread: 'omt_images' }));
        assert.equal(f.status(image1), 'queued'); assert.equal(f.status(image2), 'queued');
        const imageIds = [image1, image2].map(id => uuidFor(f, id));
        f.mode = 'images';
        await f.restart();
        await until(() => f.done(image1) && f.done(image2), 'durable image batch after complete gateway restart');
        assert.equal(f.workers.length, 3); assert.equal(f.workers[2].resumed, selected.cc);
        assert.deepEqual({ ...f.selected() }, selected);
        const batch = f.workers[2].inputs[0].batchInputs;
        assert.deepEqual(batch.map(d => d.uuid), imageIds);
        assert.equal(f.downloads.length, 2);
        assert.equal(f.requests.filter(r => r.mode === 'images').length, 2);
        assert.equal(f.events.filter(e => e.type === 'activity' && e.tool === 'Read').length, 2);
        assert.equal(await readFile(f.marker, 'utf8'), 'executed\n', 'stop/resume/restart do not replay approved operations');
        const report = await f.usage();
        assert.deepEqual(report.latest.usage, sumUsage(f.requests.filter(r => r.mode === 'images')));
        assert.deepEqual(report.latest.models, ['fixture-reported-model']);
        assert.equal(report.historyIncomplete, false); assert.equal(report.unidentifiedResults, false);
        assert.deepEqual(f.errors, []);
        assert.ok(!JSON.stringify(f.logs).includes('UNAUTHORIZED_INPUT'));
        t.diagnostic('Actual CC + pinned raw SDK ingress: one approved local operation, two attributed supplements, native compact, full-file Done barrier, stop/resume, queued image restart and two native Read calls. Feishu delivery and model intelligence are substitutes.');
    } finally { firstGate.resolve(); fileGate.resolve(); await f.close(); }
});

test('real CC/SDK passes a special question-text key through the native AskUserQuestion answer', { timeout: 90000 }, async () => {
    let asked = false, inspected = false;
    const f = await migrationFixture((_f, request) => {
        if (!asked) {
            asked = true;
            return { content: [{ type: 'tool_use', id: 'toolu_special_question', name: 'AskUserQuestion', input: {
                questions: [
                    { question: '__proto__', header: 'Property key', options: [
                        { label: 'supported', description: 'Retain the exact key.' },
                        { label: 'cancel', description: 'Stop the fixture.' }
                    ], multiSelect: false },
                    { question: 'Continue?', header: 'Confirmation', options: [
                        { label: 'yes', description: 'Continue with the answer.' },
                        { label: 'no', description: 'Stop the fixture.' }
                    ], multiSelect: false }
                ]
            } }] };
        }
        const results = (request.body.messages ?? []).flatMap(message => Array.isArray(message.content)
            ? message.content.filter(block => block.type === 'tool_result' && block.tool_use_id === 'toolu_special_question') : []);
        assert.equal(results.length, 1, 'actual native tool must supply exactly one answer result');
        const output = String(results[0].content);
        // The pinned CC renders approved answers into its native tool-result text,
        // rather than retaining the answers object as raw JSON.
        assert.ok(output.includes('"__proto__"="supported"'), 'actual native tool result must retain the special answer key');
        assert.ok(output.includes('"Continue?"="yes"'), 'ordinary question mapping must remain intact');
        inspected = true;
        return { text: 'QUESTION_KEY_PRESERVED' };
    });
    try {
        const id = await f.text('Ask the fixture questions');
        await until(() => f.requests.length > 0, 'fixture model receives native input');
        await until(() => f.sent.some(s => s.card?.header?.title?.content === '需要补充信息') ||
            f.events.some(e => e.type === 'result') || f.errors.length > 0, 'native question card or terminal result', 12000);
        const card = f.sent.find(s => s.card?.header?.title?.content === '需要补充信息');
        assert.ok(card, 'Actual CC/SDK did not open a question: ' + JSON.stringify({
            requests: f.requests.map(r => ({ number: r.number, mode: r.mode })),
            events: f.events.filter(e => ['result', 'request', 'error', 'activity'].includes(e.type)).slice(-12),
            errors: f.errors, logs: f.logs.slice(-6)
        }));
        const actions = card.card.body.elements.filter(element => element.value).map(element => element.value);
        for (let index = 0; index < 2; index++) {
            const choice = actions.find(value => value.question === index && value.option === 0);
            assert.ok(choice);
            const toast = await f.click(card, 'alice', choice);
            assert.match(toast?.toast?.content ?? '', /已记录你的选择/);
        }
        const submit = actions.find(value => value.decision === 'submit');
        assert.ok(submit);
        assert.match((await f.click(card, 'alice', submit))?.toast?.content ?? '', /已提交/);
        await until(() => f.done(id), 'native question result and final delivery');
        assert.deepEqual(f.errors, [], 'model fixture assertions must hold; actual tool results: ' +
            JSON.stringify(f.requests.slice(1).flatMap(r => (r.body.messages ?? [])
                .flatMap(m => Array.isArray(m.content) ? m.content.filter(b => b.type === 'tool_result') : []))).slice(0, 3000));
        assert.ok(inspected, 'follow-up model call must validate the serialized answer');
    } finally { await f.close(); }
});
