import test from 'node:test';
import assert from 'node:assert/strict';
import { createLarkChannel, normalize as sdkNormalize, type LarkChannel, type RawMessageEvent } from '@larksuite/channel';
import { normalize, normalizeHistoryItem, feishuTransport } from '../src/feishu.ts';
import { bindFeishuInbound } from '../src/feishu-events.ts';
import { route } from '../src/policy.ts';
import { fixture, until } from './helpers.ts';

const identity = { openId: 'ou_fixture_bot', name: 'Fixture bot' };
let sequence = 0;
function raw(type: string, content: unknown, options: { author?: string; chat?: string; mention?: boolean; reply?: string; thread?: string; dm?: boolean } = {}): RawMessageEvent {
    return {
        sender: { sender_id: { open_id: options.author ?? 'alice' }, sender_type: 'user' },
        message: { message_id: `om_fixture_${++sequence}`, chat_id: options.chat ?? 'group1', chat_type: options.dm ? 'p2p' : 'group',
            message_type: type, content: typeof content === 'string' ? content : JSON.stringify(content), create_time: String(Date.now()),
            parent_id: options.reply, thread_id: options.thread,
            mentions: options.mention === false ? [] : [{ key: '@_user_1', id: { open_id: identity.openId }, name: identity.name }] }
    };
}
async function adapt(event: RawMessageEvent) {
    return normalize(await sdkNormalize(event, { botIdentity: identity, stripBotMentions: true, includeRaw: true }));
}
// This test harness reaches the pinned SDK dispatcher to replace only network ingress.
// No production code accesses these private fields or bypasses the SDK normalizer.
function dispatcher(channel: LarkChannel) {
    const sdk = channel as unknown as { registerDispatcherHandlers(): void; safety: { setBotIdentity(value: typeof identity): void; dispose(): Promise<void> }; dispatcher: { handles: Map<string, (event: unknown) => Promise<unknown>> } };
    channel.botIdentity = identity; sdk.safety.setBotIdentity(identity); sdk.registerDispatcherHandlers();
    return { receive: (event: RawMessageEvent) => sdk.dispatcher.handles.get('im.message.receive_v1')!(event), dispose: () => sdk.safety.dispose() };
}

test('real pinned SDK and production adapter preserve raw text, identities, explicit mention, quote and thread', async () => {
    const event = raw('text', { text: '@_user_1 hello from Bob' }, { author: 'bob', reply: 'om_quote', thread: 'omt_fixture' });
    const value = await adapt(event);
    assert.equal(value.id, event.message.message_id); assert.equal(value.chatId, 'group1');
    assert.equal(value.senderId, 'bob'); assert.equal(value.senderIsBot, false);
    assert.equal(value.mentioned, true); assert.equal(value.replyTo, 'om_quote'); assert.equal(value.threadId, 'omt_fixture');
    assert.equal(value.text, 'hello from Bob'); assert.equal(value.time, Number(event.message.create_time));
    assert.deepEqual(value.resources, []); assert.ok(!('raw' in value));
    assert.equal((await adapt(raw('text', { text: '@Fixture bot is just text' }, { mention: false }))).mentioned, false);
    assert.equal((await adapt(raw('text', { text: 'reply without mention' }, { mention: false, reply: 'om_bot_reply' }))).mentioned, false);
    assert.equal((await adapt(raw('text', { text: 'private text' }, { dm: true, mention: false }))).chatType, 'p2p');
});

test('raw standalone images and files retain downloadable resource metadata', async () => {
    const image = await adapt(raw('image', { image_key: 'img_fixture_one' }));
    assert.deepEqual(image.resources, [{ key: 'img_fixture_one', kind: 'image', name: 'image-_fixture_one' }]);
    assert.equal(image.imageOnly, true);
    const file = await adapt(raw('file', { file_key: 'file_fixture_one', file_name: '资料.txt' }));
    assert.deepEqual(file.resources, [{ key: 'file_fixture_one', kind: 'file', name: '资料.txt' }]);
    assert.match(file.text, /资料/); assert.equal(file.imageOnly, false);
});

test('pinned post parser already supports locale bodies, content_v2, inline media and attachment zones', async () => {
    for (const locale of [false, true]) {
        const body = { title: 'Fixture title', content: [[{ tag: 'text', text: 'obsolete body' }]], content_v2: [[
            { tag: 'at', user_id: identity.openId }, { tag: 'text', text: 'visible body' },
            { tag: 'img', image_key: 'img_fixture_rich' }, { tag: 'media', file_key: 'file_fixture_media' }
        ]] };
        const input = { ...(locale ? { zh_cn: body } : body), files: [{ file_key: 'file_fixture_zone', file_name: 'zone.txt', is_folder: false }] };
        const value = await adapt(raw('post', input));
        assert.match(value.text, /Fixture title/); assert.match(value.text, /visible body/);
        assert.ok(!value.text.includes('obsolete body')); assert.equal(value.mentioned, true);
        assert.equal(value.inputIssue, undefined); assert.equal(value.imageOnly, false);
        assert.deepEqual(value.resources.map(r => [r.key, r.kind]), [
            ['img_fixture_rich', 'image'], ['file_fixture_media', 'file'], ['file_fixture_zone', 'file']
        ]);
        assert.equal(value.resources[2]!.name, 'zone.txt');
    }
});

test('missing image/file content and unsupported post elements are explicitly incomplete, never full model inputs', async () => {
    const f = await fixture();
    try {
        for (const event of [
            raw('image', {}), raw('image', { image_key: 17 }), raw('file', { file_name: 'missing-key.txt' }), raw('text', '{invalid json'),
            raw('post', { content: [[{ tag: 'text', text: 'only part' }, { tag: 'fixture_unknown', payload: 'unreadable' }]] }),
            raw('post', { content: [[{ tag: 'text', text: 'missing image' }, { tag: 'img' }]] }),
            raw('post', { files: [{ file_key: 'folder_fixture', is_folder: true }] })
        ]) {
            const value = await adapt(event);
            assert.ok(value.inputIssue, 'the production adapter must not treat a placeholder/partial body as complete');
            await f.gateway.message(value);
            assert.equal(f.workers.length, 0); assert.equal(f.transport.receipts.length, 0);
            assert.ok(f.transport.sent.some(s => s.replyTo === value.id && s.text?.includes('未提交')));
        }
        const before = f.transport.sent.length;
        await f.gateway.message(await adapt(raw('image', {}, { author: 'stranger' })));
        await f.gateway.message(await adapt(raw('image', {}, { mention: false })));
        assert.equal(f.transport.sent.length, before, 'unsupported-format feedback is also behind authorization');
    } finally { await f.close(); }
});

test('historical raw resources and quotes remain scoped by chat and author before normalizing content', async () => {
    const f = await fixture();
    try {
        const trigger = await adapt(raw('text', { text: '@_user_1 use quote' }, { reply: 'om_history' }));
        const room = route(f.config, trigger)!;
        const item = { message_id: 'om_history', chat_id: 'group1', sender: { id: 'bob', sender_type: 'user' },
            msg_type: 'post', create_time: String(Date.now()), parent_id: 'om_parent', thread_id: 'omt_fixture',
            body: { content: JSON.stringify({ content: [[{ tag: 'text', text: 'quoted file' }]], files: [{ file_key: 'file_history', file_name: 'quote.txt' }] }) } };
        const quoted = await normalizeHistoryItem(item, room, identity.openId, 'cli_fixture');
        assert.ok(quoted); assert.equal(quoted.senderId, 'bob'); assert.equal(quoted.replyTo, 'om_parent');
        assert.equal(quoted.threadId, 'omt_fixture'); assert.equal(quoted.resources[0]?.key, 'file_history');
        assert.equal(await normalizeHistoryItem({ ...item, chat_id: 'foreign' }, room, identity.openId), undefined);
        assert.equal(await normalizeHistoryItem({ ...item, sender: { id: 'stranger', sender_type: 'user' } }, room, identity.openId), undefined);
        f.transport.quoted.set('om_history', quoted);
        await f.gateway.message(trigger); await until(() => f.workers[0]?.sent.length === 1);
        assert.equal(f.workers[0]!.sent[0]!.quote?.resources[0]?.key, 'file_history');
        assert.ok(!JSON.stringify(f.workers[0]!.sent).includes('"raw":'));
    } finally { await f.close(); }
});

test('production channel blocks merge-forward prefetch while explicit scoped quote reads still work', async () => {
    const f = await fixture();
    const env = 'RAW_EVENT_FIXTURE_SECRET', before = process.env[env];
    f.config.appSecretEnv = env; process.env[env] = 'fixture-not-real';
    let reads = 0;
    const logs: object[] = [];
    const { channel, transport } = feishuTransport(f.config, (event, detail) => logs.push({ event, detail }), options => {
        const created = createLarkChannel(options);
        // Install the HTTP substitute before the adapter captures its scoped reader.
        created.rawClient.im.v1.message.get = async () => { reads++; return { code: 0, data: { items: [
            { message_id: 'om_scoped_quote', chat_id: 'group1', sender: { id: 'bob', sender_type: 'user' }, msg_type: 'text', body: { content: '{"text":"scoped quote"}' } }
        ] } }; };
        return created;
    });
    const sdk = dispatcher(channel);
    bindFeishuInbound(channel, f.gateway, (event, detail) => logs.push({ event, detail }), () => false);
    try {
        await sdk.receive(raw('merge_forward', {}, { author: 'stranger' }));
        assert.equal(reads, 0, 'no nested message API read before author authorization');
        assert.equal(f.workers.length, 0); assert.equal(f.transport.sent.length, 0);
        assert.equal(f.store.db.prepare('SELECT count(*) AS n FROM inbox').get()!.n, 0);
        const authorized = raw('merge_forward', {});
        await sdk.receive(authorized);
        await until(() => f.transport.sent.some(s => s.replyTo === authorized.message.message_id));
        assert.equal(reads, 0); assert.equal(f.workers.length, 0);
        assert.match(f.transport.sent.at(-1)!.text!, /合并转发不会自动展开/);
        const input = await adapt(raw('text', { text: '@_user_1 quoted' })), room = route(f.config, input)!;
        assert.equal((await transport.fetch('om_scoped_quote', room))?.text, 'scoped quote');
        assert.equal(reads, 1);
        assert.equal(await transport.fetch('om_scoped_quote', { ...room, chatId: 'foreign' }), undefined);
        assert.equal(reads, 1);
        assert.ok(!JSON.stringify(logs).includes('fixture-not-real'));
    } finally {
        await sdk.dispose(); await channel.disconnect();
        if (before === undefined) delete process.env[env]; else process.env[env] = before;
        await f.close();
    }
});

test('production subscription carries raw rich resources to the gateway without storing raw payloads', async () => {
    const f = await fixture();
    const env = 'RAW_WIRING_FIXTURE_SECRET', before = process.env[env];
    f.config.appSecretEnv = env; process.env[env] = 'fixture-not-real';
    const logs: object[] = [];
    const { channel } = feishuTransport(f.config, (event, detail) => logs.push({ event, detail }));
    const sdk = dispatcher(channel);
    let closed = false;
    bindFeishuInbound(channel, f.gateway, (event, detail) => logs.push({ event, detail }), () => closed);
    try {
        const event = raw('post', { content: [[{ tag: 'at', user_id: identity.openId }, { tag: 'text', text: 'actual rich input' },
            { tag: 'img', image_key: 'img_wiring' }]], files: [{ file_key: 'file_wiring', file_name: 'fixture.txt' }], unused_vendor_payload: 'RAW_ONLY_SENTINEL' });
        await sdk.receive(event); await until(() => f.workers[0]?.sent.length === 1);
        const delivery = f.workers[0]!.sent[0]!;
        assert.equal(delivery.message.id, event.message.message_id); assert.equal(delivery.message.senderId, 'alice');
        assert.equal(delivery.message.resources.length, 2); assert.equal(delivery.attachments.length, 2);
        assert.match(delivery.message.text, /actual rich input/);
        const persisted = f.store.db.prepare('SELECT message,payload FROM inbox').all();
        assert.ok(!JSON.stringify(persisted).includes('RAW_ONLY_SENTINEL'));
        assert.ok(!JSON.stringify(logs).includes('RAW_ONLY_SENTINEL'));
        assert.ok(!JSON.stringify(delivery).includes('"raw":'));
        closed = true;
        await sdk.receive(raw('text', { text: '@_user_1 ignored after shutdown' }));
        assert.equal(f.workers[0]!.sent.length, 1);
    } finally {
        await sdk.dispose(); await channel.disconnect();
        if (before === undefined) delete process.env[env]; else process.env[env] = before;
        await f.close();
    }
});
