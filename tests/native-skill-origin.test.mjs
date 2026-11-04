import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { migrationFixture, until, deferred } from './migration-fixture.mjs';

test('real native Skill injection keeps streamed thinking bound to its original human input before the final result', { timeout: 90000 }, async t => {
    const gate = deferred();
    let issuedSkill = false;
    const f = await migrationFixture((_f, request) => {
        if (!issuedSkill) {
            issuedSkill = true;
            assert.ok(JSON.stringify(request.body).includes('fixture-origin-skill'), 'native skill must be discovered');
            return { content: [{ type: 'tool_use', id: 'toolu_skill_origin', name: 'Skill', input: { skill: 'fixture-origin-skill' } }] };
        }
        assert.ok(JSON.stringify(request.body.messages).includes('FIXTURE_SKILL_ORIGIN_MARKER'), 'real native Skill tool must inject the fixture skill');
        return { content: [{ type: 'thinking', thinking: 'FIXTURE_SKILL_THINKING' }, { type: 'text', text: 'FIXTURE_SKILL_ANSWER' }], afterBlock: index => index === 0 ? gate.promise : undefined };
    });
    try {
        const configRoot = join(f.root, 'trusted-fixture');
        await mkdir(join(configRoot, 'skills', 'fixture-origin-skill'), { recursive: true });
        await writeFile(join(configRoot, 'skills', 'fixture-origin-skill', 'SKILL.md'), '---\nname: fixture-origin-skill\ndescription: Fixture origin tracing skill.\n---\nFIXTURE_SKILL_ORIGIN_MARKER. Continue the original human request.\n');
        f.config.projects[0].trustedConfigDir = configRoot;
        f.config.projects[0].allowedTools = ['Skill'];
        if (process.env.FEISHU_TEST_CLAUDE_EXECUTABLE) f.config.runtime.claudeExecutable = process.env.FEISHU_TEST_CLAUDE_EXECUTABLE;
        const id = await f.text('Invoke fixture-origin-skill and then stream the fixture answer.');
        await until(() => f.events.some(event => event.type === 'thinking' && event.text.includes('FIXTURE_SKILL_THINKING')), 'post-Skill streamed thinking');
        const uuid = f.store.db.prepare('SELECT uuid FROM inbox WHERE id=?').get(id).uuid;
        const thinking = f.events.filter(event => event.type === 'thinking' && event.text.includes('FIXTURE_SKILL_THINKING'));
        t.diagnostic(JSON.stringify({ inputUuid: uuid, thinkingOrigins: thinking.map(event => event.turnId), eventTypes: f.events.map(event => event.type) }));
        assert.ok(thinking.every(event => event.turnId === uuid), 'native Skill synthetic input must not replace the gateway human origin');
        await until(() => f.sent.some(sent => sent.replyTo === id && JSON.stringify(sent.card).includes('FIXTURE_SKILL_THINKING')) || f.updates.some(update => JSON.stringify(update.card).includes('FIXTURE_SKILL_THINKING')), 'thinking preview must appear before final result', 5000);
        assert.ok(!f.events.some(event => event.type === 'result'), 'preview must not depend on terminal origin repair');
        gate.resolve();
        await until(() => f.done(id), 'post-Skill final delivery');
        assert.deepEqual(f.errors, []);
    } finally { gate.resolve(); await f.close(); }
});
