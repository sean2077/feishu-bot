import test from 'node:test';
import assert from 'node:assert/strict';
import { createLarkChannel, type LarkChannel } from '@larksuite/channel';
import { bindFeishuInbound } from '../src/feishu-events.ts';
import { route } from '../src/policy.ts';
import type { Question } from '../src/approvals.ts';
import { fixture, message, until } from './helpers.ts';

/** Test-only access to the pinned real SDK dispatcher, not an unchecked production raw handler. */
function dispatcher(channel: LarkChannel) {
    const sdk = channel as unknown as {
        registerDispatcherHandlers(): void;
        dispatcher: { handles: Map<string, (event: unknown) => Promise<unknown>> };
        safety: { setBotIdentity(value: { openId: string; name: string }): void; dispose(): Promise<void> };
    };
    const identity = { openId: 'ou_fixture_bot', name: 'Fixture bot' };
    channel.botIdentity = identity;
    sdk.safety.setBotIdentity(identity);
    sdk.registerDispatcherHandlers();
    return {
        receive: (raw: unknown) => sdk.dispatcher.handles.get('card.action.trigger')!(raw),
        dispose: () => sdk.safety.dispose()
    };
}
type Card = { body: { elements: { value?: Record<string, unknown> }[] } };
function title(card: object | undefined): string | undefined {
    return (card as { header?: { title?: { content?: string } } } | undefined)?.header?.title?.content;
}
async function questionHarness(questions: Question[]) {
    const f = await fixture();
    const channel = createLarkChannel({
        appId: 'cli_fixture', appSecret: 'fictional-secret',
        policy: { groupAllowlist: ['group1'], dmMode: 'allowlist', dmAllowlist: [], requireMention: true },
        // Match the production card lane. Changing this cannot fix the dedup identity.
        safety: { chatQueue: { enabled: true, cardActions: 'separate', mergeWhileBusy: false },
            batch: { text: { delayMs: 0, longDelayMs: 0, maxMessages: 1 }, media: { delayMs: 0, maxItems: 1 } } }
    });
    const sdk = dispatcher(channel);
    bindFeishuInbound(channel, f.gateway, () => {}, () => false);
    const room = route(f.config, message())!, session = f.store.session(room.key);
    const pending = f.gateway.approvals.request(room, session.id, 'AskUserQuestion', {}, 'Fixture questions', undefined, questions);
    await until(() => f.transport.sent.some(s => title(s.card) === '需要补充信息'));
    const card = f.transport.sent.find(s => title(s.card) === '需要补充信息')!;
    await new Promise(resolve => setImmediate(resolve)); // remote card ID is registered in Approvals
    const elements = (card.card as Card).body.elements;
    const button = (predicate: (v: Record<string, unknown>) => boolean) => elements.find(e => e.value && predicate(e.value))!.value!;
    const submit = button(v => v.decision === 'submit');
    const requestId = String(submit.request);
    const click = (value: unknown, token: string, operator = 'alice', chatId = room.chatId, messageId = card.id) =>
        sdk.receive({ schema: '2.0', event_type: 'card.action.trigger', token,
            context: { open_message_id: messageId, open_chat_id: chatId },
            operator: { open_id: operator }, action: { tag: 'button', value } });
    return {
        f, room, session, pending, button, submit, requestId, click,
        close: async () => { await sdk.dispose(); await channel.disconnect(); await f.close(); }
    };
}

test('pinned SDK 0.7.1 drops distinct-token repeat submit; scoped /answer restores the deferred question', async () => {
    const h = await questionHarness([
        { question: 'Choose color', options: [{ label: 'red' }, { label: 'blue' }] },
        { question: 'Choose size', options: [{ label: 'small' }, { label: 'large' }] }
    ]);
    try {
        const { f, room, click, submit, button, requestId } = h;
        const early = await click(submit, 'delivery-early');
        assert.deepEqual(early, { toast: { type: 'info', content: '请回答所有问题' } }, 'production gateway receives the first click');
        const color = button(v => v.question === 0 && v.option === 0);
        const size = button(v => v.question === 1 && v.option === 0);
        assert.match(JSON.stringify(await click(color, 'delivery-color')), /已记录你的选择/);
        assert.match(JSON.stringify(await click(size, 'delivery-size')), /已记录你的选择/);
        assert.equal(await click(submit, 'delivery-early'), undefined, 'same delivery replay is dropped');
        assert.equal(await click(submit, 'delivery-new-after-completion'), undefined,
            'KNOWN SDK LIMIT: a genuine new platform delivery is also dropped by the action-value dedup key');
        assert.equal(f.store.db.prepare('SELECT state FROM requests WHERE id=?').get(requestId)!.state, 'pending');
        assert.equal(f.workers.length, 0, 'no new CC turn or implicit answer may be fabricated');

        // The SDK card listener is not the auth boundary for /answer; the normal authorized
        // Feishu message route and request/chat ownership still decide who can resolve it.
        const originalCount = f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n;
        await f.gateway.message(message({ senderId: 'stranger', text: `/answer ${requestId} untrusted` }));
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n, originalCount);
        await f.gateway.message(message({ senderId: 'bob', chatId: 'group2',
            text: `/answer ${requestId} cross-chat` }));
        assert.equal(f.store.db.prepare('SELECT state FROM requests WHERE id=?').get(requestId)!.state, 'pending');
        assert.deepEqual(await click(submit, 'delivery-foreign-chat', 'bob', 'group2'), {});
        assert.deepEqual(await click(submit, 'delivery-unauthorized-user', 'stranger'), {});
        assert.equal(f.store.db.prepare('SELECT state FROM requests WHERE id=?').get(requestId)!.state, 'pending');

        const answers = { 'Choose color': 'red', 'Choose size': 'small' };
        await f.gateway.message(message({ senderId: 'alice', chatId: room.chatId,
            text: `/answer ${requestId} ${JSON.stringify(answers)}` }));
        const outcome = await h.pending;
        assert.equal(outcome.approved, true);
        assert.deepEqual(outcome.answers, answers);
        assert.equal(f.store.db.prepare('SELECT state FROM requests WHERE id=?').get(requestId)!.state, 'approved');
        assert.equal(f.workers.length, 0, 'fallback does not start another native conversation');
        assert.equal(f.store.session(room.key).id, h.session.id);
    } finally { await h.close(); }
});

test('pinned SDK 0.7.1 also drops a second multi-select option click even with a new token', async () => {
    const h = await questionHarness([{ question: 'Select letters', multiSelect: true,
        options: [{ label: 'A' }, { label: 'B' }] }]);
    try {
        const option = h.button(v => v.question === 0 && v.option === 0);
        assert.match(JSON.stringify(await h.click(option, 'first-select')), /已记录你的选择.*A/);
        assert.equal(await h.click(option, 'second-click-to-toggle-off'), undefined,
            'KNOWN SDK LIMIT: new delivery of the same option cannot toggle the saved selection');
        assert.equal(h.f.store.db.prepare('SELECT state FROM requests WHERE id=?').get(h.requestId)!.state, 'pending');
        const replacement = 'B instead of A';
        await h.f.gateway.message(message({ senderId: 'alice',
            text: `/answer ${h.requestId} ${replacement}` }));
        assert.deepEqual((await h.pending).answers, { 'Select letters': replacement });
    } finally { await h.close(); }
});
