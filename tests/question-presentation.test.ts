import test from 'node:test';
import assert from 'node:assert/strict';
import { fixture, message, until } from './helpers.ts';
import type { Question } from '../src/approvals.ts';

test('question cards explain choices and selection mode while keeping flat actor-scoped actions', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const questions: Question[] = [
            { header: '执行方式', question: '如何检查？', options: [
                { label: '只读检查', description: '检查配置，不执行任何外部写入。' },
                { label: '运行测试', description: '在工作区运行已授权测试。' }
            ] },
            { header: '检查范围', question: '检查哪些部分？', multiSelect: true,
                options: [{ label: '消息', description: '验证明确提及。' }, { label: '文件', description: '验证附件归属。' }] }
        ];
        const outcome = f.gateway.approvals.request(worker.room, worker.session.id, 'AskUserQuestion', {}, 'fixture', undefined, questions);
        await until(() => f.transport.sent.some(item => JSON.stringify(item.card).includes('需要补充信息')));
        const sent = f.transport.sent.find(item => JSON.stringify(item.card).includes('需要补充信息'))!;
        const card = sent.card as { body: { elements: { tag: string; content?: string; value?: { request: string } }[] } };
        const text = card.body.elements.filter(e => e.tag === 'markdown').map(e => e.content).join('\n');
        for (const expected of ['执行方式', '检查范围', '单选', '多选', '检查配置，不执行任何外部写入。', '验证附件归属。'])
            assert.ok(text.includes(expected), `missing decision context: ${expected}`);
        assert.ok(card.body.elements.every(e => e.tag === 'markdown' || e.tag === 'button'), 'do not nest actions into columns');
        const id = card.body.elements.find(e => e.tag === 'button')!.value!.request;
        await f.gateway.approvals.click('group1', 'alice', { request: id, question: 0, option: 0 }, sent.id);
        assert.equal(await f.gateway.approvals.click('group1', 'bob', { request: id, decision: 'submit' }, sent.id), '请回答所有问题');
        await f.gateway.approvals.click('group1', 'alice', { request: id, question: 1, option: 1 }, sent.id);
        assert.equal(await f.gateway.approvals.click('group1', 'alice', { request: id, decision: 'submit' }, sent.id), '已提交');
        assert.deepEqual((await outcome).answers, { '如何检查？': '只读检查', '检查哪些部分？': '文件' });
    } finally { await f.close(); }
});

test('malformed optional question metadata is rejected before creating a card', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        for (const question of [
            { question: 'fixture', header: 42, options: [{ label: 'ok' }] },
            { question: 'fixture', options: [{ label: 'ok', description: { invalid: true } }] }
        ]) {
            const before = f.transport.sent.length;
            let timer: ReturnType<typeof setTimeout> | undefined;
            const outcome = f.gateway.approvals.request(worker.room, worker.session.id, 'AskUserQuestion', {}, 'fixture', undefined, [question as unknown as Question]);
            const result = await Promise.race([outcome, new Promise<null>(resolve => { timer = setTimeout(() => resolve(null), 500); })]);
            clearTimeout(timer);
            assert.deepEqual(result, { approved: false, reason: 'unsupported question shape' });
            assert.equal(f.transport.sent.length, before);
        }
    } finally { await f.close(); }
});

test('repeated question text cannot silently overwrite a distinct answer, even with different headers', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const before = f.transport.sent.length;
        const questions: Question[] = [
            { header: 'First environment', question: 'Which choice?', options: [{ label: 'development' }, { label: 'release' }] },
            { header: 'Second environment', question: 'Which choice?', options: [{ label: 'staging' }, { label: 'production' }] }
        ];
        // Native SDK AskUserQuestion resolves with Record<question text, answer>.
        // Distinct answers for the same key cannot both survive serialization.
        let timer: ReturnType<typeof setTimeout> | undefined;
        const outcome = f.gateway.approvals.request(worker.room, worker.session.id,
            'AskUserQuestion', { questions }, 'fixture', undefined, questions);
        const result = await Promise.race([
            outcome, new Promise<null>(resolve => { timer = setTimeout(() => resolve(null), 300); })
        ]);
        clearTimeout(timer);
        assert.deepEqual(result, { approved: false, reason: 'unsupported question shape' });
        assert.equal(f.transport.sent.length, before, 'do not present an unanswerable or lossy card');
        assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM requests WHERE state='pending'").get()!.n, 0);
    } finally { await f.close(); }
});

test('native question key named __proto__ survives card selection as an own JSON answer', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        const questions: Question[] = [
            { question: '__proto__', options: [{ label: 'supported' }] },
            { question: 'Continue?', options: [{ label: 'yes' }] }
        ];
        const outcome = f.gateway.approvals.request(worker.room, worker.session.id,
            'AskUserQuestion', { questions }, 'fixture', undefined, questions);
        await until(() => f.transport.sent.some(item => JSON.stringify(item.card).includes('需要补充信息')));
        const card = f.transport.sent.find(item => JSON.stringify(item.card).includes('需要补充信息'))!;
        await until(() => !!f.store.db.prepare('SELECT 1 FROM outputs WHERE id=?').get(card.id));
        const requestId = (card.card as { body: { elements: { value?: { request: string } }[] } })
            .body.elements.find(element => element.value)?.value?.request;
        assert.ok(requestId);
        for (let index = 0; index < questions.length; index++)
            assert.match(await f.gateway.approvals.click('group1', 'alice',
                { request: requestId, question: index, option: 0 }, card.id), /已记录你的选择/);
        assert.equal(await f.gateway.approvals.click('group1', 'alice',
            { request: requestId, decision: 'submit' }, card.id), '已提交');
        const result = await outcome;
        assert.equal(result.approved, true);
        assert.equal(Object.hasOwn(result.answers!, '__proto__'), true,
            'a valid native question must not vanish through the Object.prototype setter');
        assert.equal(JSON.stringify(result.answers), '{"__proto__":"supported","Continue?":"yes"}');
    } finally { await f.close(); }
});

test('options with identical answer labels are rejected before presenting a lossy native question', async () => {
    const f = await fixture();
    try {
        await f.gateway.message(message());
        await until(() => f.workers[0]?.sent.length === 1);
        const worker = f.workers[0]!;
        for (const multiSelect of [false, true]) {
            const questions: Question[] = [{
                question: 'How should the operation proceed?',
                multiSelect,
                options: [
                    { label: 'Run checks', description: 'Inspect without modifying state.' },
                    { label: 'Run checks', description: 'Apply the requested changes.' }
                ]
            }];
            const before = f.transport.sent.length;
            let timer: ReturnType<typeof setTimeout> | undefined;
            const outcome = f.gateway.approvals.request(worker.room, worker.session.id,
                'AskUserQuestion', { questions }, 'fixture', undefined, questions);
            const result = await Promise.race([
                outcome, new Promise<null>(resolve => { timer = setTimeout(() => resolve(null), 300); })
            ]);
            clearTimeout(timer);
            assert.deepEqual(result, { approved: false, reason: 'unsupported question shape' },
                'two semantically different options must not serialize to the same native answer');
            assert.equal(f.transport.sent.length, before);
            assert.equal(f.store.db.prepare("SELECT count(*) AS n FROM requests WHERE state='pending'").get()!.n, 0);
        }
    } finally { await f.close(); }
});
