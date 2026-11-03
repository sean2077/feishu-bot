import test from 'node:test';
import assert from 'node:assert/strict';
import { createLarkChannel, type LarkChannel } from '@larksuite/channel';
import { bindFeishuInbound } from '../src/feishu-events.ts';
import { route } from '../src/policy.ts';
import { fixture, message, until } from './helpers.ts';

// This harness invokes the real pinned Channel SDK's registered event dispatcher,
// not a raw listener that bypasses SDK safety. Production never accesses these fields.
function cardDispatcher(channel: LarkChannel) {
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

test('a new Feishu delivery of submit reaches the same pending question after early incomplete feedback', async () => {
    const f = await fixture();
    const channel = createLarkChannel({
        appId: 'cli_fixture', appSecret: 'fictional-secret',
        policy: { groupAllowlist: ['group1'], dmMode: 'allowlist', dmAllowlist: [], requireMention: true },
        safety: { chatQueue: { enabled: true, cardActions: 'separate', mergeWhileBusy: false },
            batch: { text: { delayMs: 0, longDelayMs: 0, maxMessages: 1 }, media: { delayMs: 0, maxItems: 1 } } }
    });
    const sdk = cardDispatcher(channel);
    bindFeishuInbound(channel, f.gateway, () => {}, () => false);
    try {
        const room = route(f.config, message())!, session = f.store.session(room.key);
        const pending = f.gateway.approvals.request(room, session.id, 'AskUserQuestion', {}, 'Fixture questions', undefined, [
            { question: 'Choose color', options: [{ label: 'red' }, { label: 'blue' }] },
            { question: 'Choose size', options: [{ label: 'small' }, { label: 'large' }] }
        ]);
        await until(() => f.transport.sent.some(s => !!s.card && s.card.header.title.content === '需要补充信息'));
        const card = f.transport.sent.find(s => !!s.card && s.card.header.title.content === '需要补充信息')!;
        // Await the adapter's asynchronous registration of the successfully created remote card.
        await new Promise(resolve => setImmediate(resolve));
        const items = (card.card as { body: { elements: { value?: Record<string, unknown> }[] } }).body.elements;
        const submit = items.find(e => e.value?.decision === 'submit')!.value!;
        const color = items.find(e => e.value?.question === 0 && e.value?.option === 0)!.value!;
        const size = items.find(e => e.value?.question === 1 && e.value?.option === 0)!.value!;
        const click = (value: unknown, token: string) => sdk.receive({
            schema: '2.0', event_type: 'card.action.trigger', token,
            context: { open_message_id: card.id, open_chat_id: room.chatId },
            operator: { open_id: 'alice' }, action: { tag: 'button', value }
        });
        assert.deepEqual(await click(submit, 'distinct-delivery-early'), { toast: { type: 'info', content: '请回答所有问题' } });
        assert.match(JSON.stringify(await click(color, 'distinct-delivery-color')), /已记录你的选择/);
        assert.match(JSON.stringify(await click(size, 'distinct-delivery-size')), /已记录你的选择/);
        // This is a NEW platform delivery, not retransmission of the first click.
        assert.deepEqual(await click(submit, 'distinct-delivery-complete'), { toast: { type: 'info', content: '已提交' } });
        const result = await pending;
        assert.equal(result.approved, true);
        assert.deepEqual(result.answers, { 'Choose color': 'red', 'Choose size': 'small' });
        assert.equal(f.store.db.prepare('SELECT state FROM requests').get()!.state, 'approved');
    } finally {
        await sdk.dispose();
        await channel.disconnect();
        await f.close();
    }
});
