import { createLarkChannel, normalize as normalizeEvent, type LarkChannel, type NormalizedMessage, type RawMessageEvent } from '@larksuite/channel';
import { readFile, stat } from 'node:fs/promises';
import { readable } from './policy.ts';
import type { Config, Message, Resource, Transport, Room, ReceiptState } from './types.ts';
export function normalize(msg: NormalizedMessage): Message {
    return { id: msg.messageId, chatId: msg.chatId, chatType: msg.chatType, senderId: msg.senderId, senderName: msg.senderName,
        senderIsBot: msg.senderType !== 'user', text: msg.rawContentType === 'merge_forward' ? '[合并转发未自动展开；可按需读取当前聊天的原始消息。]' : msg.content, time: msg.createTime,
        mentioned: msg.mentionedBot, replyTo: msg.replyToMessageId, threadId: msg.threadId,
        resources: (msg.rawContentType === 'merge_forward' ? [] : msg.resources).filter(r => ['image', 'file', 'audio', 'video'].includes(r.type)).map(r => ({ key: r.fileKey, kind: r.type === 'image' ? 'image' : 'file', name: r.fileName ?? `${r.type}-${r.fileKey.slice(-12)}` })) };
}
export function feishuTransport(config: Config, log: (event: string, detail: object) => void): {
    transport: Transport;
    channel: ReturnType<typeof createLarkChannel>;
} {
    const secret = process.env[config.appSecretEnv];
    if (!secret)
        throw new Error(`missing Feishu credential ${config.appSecretEnv}`);
    const channel = createLarkChannel({ appId: config.appId, appSecret: secret, resolveSenderNames: false, includeRawEvent: false,
        respectProxyEnv: true, httpTimeoutMs: 20000,
        policy: { requireMention: config.limits.backgroundMessages === 0, respondToMentionAll: false, groupAllowlist: config.groups.length ? config.groups.map(g => g.chatId) : ['__disabled__'], dmMode: 'allowlist', dmAllowlist: config.dms.map(d => d.userId) },
        safety: { chatQueue: { enabled: true, mergeWhileBusy: false, cardActions: 'separate' }, batch: { text: { delayMs: 0, longDelayMs: 0, maxMessages: 1 }, media: { delayMs: 0, maxItems: 1 } } },
        logger: { debug: () => { }, trace: () => { }, info: () => { }, warn: () => log('feishu_warning', {}), error: () => log('feishu_sdk_error', {}) },
        // Do not blindly retry message creation after an ambiguous network failure.
        // Known-ID card updates are retried through the durable application outbox.
        outbound: { retry: { maxAttempts: 1, baseDelayMs: 500 }, ssrfGuard: true } });
    const receipt = reactionHandler(channel, config.appId);
    const transport: Transport = {
        send: async (room, text, replyTo, threadId) => (await channel.send(room.chatId, { markdown: text }, { replyTo, replyInThread: !!threadId })).messageId,
        card: async (room, card, replyTo, threadId) => (await channel.send(room.chatId, { card }, { replyTo, replyInThread: !!threadId })).messageId,
        update: async (id, card) => channel.updateCard(id, card),
        download: async (id, resource) => {
            const response = await channel.rawClient.im.v1.messageResource.get({ path: { message_id: id, file_key: resource.key }, params: { type: resource.kind } });
            const stream = response.getReadableStream() as import('node:stream').Readable;
            const timer = setTimeout(() => stream.destroy(new Error('attachment download timed out')), 60000);
            timer.unref();
            const chunks: Buffer[] = [];
            let total = 0;
            try {
                for await (const chunk of stream) {
                    const data = Buffer.from(chunk);
                    total += data.length;
                    if (total > config.limits.attachmentBytes)
                        throw new Error('attachment exceeds configured limit');
                    chunks.push(data);
                }
                return Buffer.concat(chunks);
            }
            finally {
                clearTimeout(timer);
                stream.destroy();
            }
        },
        fetch: async (id, room) => {
            if (!room) return undefined;
            const response = await channel.rawClient.im.v1.message.get({ path: { message_id: id } });
            if (response.code !== 0 && response.code !== undefined) throw new Error(`Message API error ${response.code}`);
            const item = response.data?.items?.find(m => m.message_id === id);
            return item ? normalizeHistoryItem(item, room, channel.botIdentity?.openId, config.appId) : undefined;
        },
        history: async (room, query) => {
            const response = await channel.rawClient.im.v1.message.list({ params: {
                container_id_type: query.threadId ? 'thread' : 'chat', container_id: query.threadId ?? room.chatId,
                start_time: query.startTime === undefined ? undefined : String(query.startTime),
                end_time: query.endTime === undefined ? undefined : String(query.endTime),
                sort_type: 'ByCreateTimeDesc', page_size: query.limit, page_token: query.pageToken,
                with_sender_name: true
            } });
            if ((response.code !== undefined && response.code !== 0) || !response.data)
                throw new Error(`History API unavailable (code ${response.code ?? 'unknown'}); check message-read permissions`);
            const data = response.data;
            if (data.has_more && !data.page_token) throw new Error('History API returned an incomplete cursor');
            const messages = await Promise.all((data.items ?? []).map(item => normalizeHistoryItem(item, room, channel.botIdentity?.openId, config.appId)));
            return { messages: messages.filter((m): m is Message => !!m), nextPageToken: data.has_more ? data.page_token : undefined };
        },
        receipt,
        file: async (room, path, name) => { if ((await stat(path)).size > config.limits.attachmentBytes)
            throw new Error('outbound file exceeds limit'); const bytes = await readFile(path); if (bytes.length > config.limits.attachmentBytes)
            throw new Error('outbound file exceeds limit'); return (await channel.send(room.chatId, { file: { source: bytes, fileName: name } })).messageId; }
    };
    return { transport, channel };
}


type ApiItem = NonNullable<NonNullable<Awaited<ReturnType<LarkChannel['rawClient']['im']['v1']['message']['list']>>['data']>['items']>[number];
/** Preserve API ownership fields: the SDK fetchMessage helper drops chat_id in 0.7.1. */
export async function normalizeHistoryItem(item: ApiItem, room: Room, botOpenId?: string, appId?: string): Promise<Message | undefined> {
    if (!item.message_id || item.chat_id !== room.chatId || item.deleted || !item.sender?.id) return undefined;
    const bot = item.sender.sender_type !== 'user';
    const self = bot && (!!botOpenId && (item.sender.id === botOpenId || item.sender.open_bot_id === botOpenId) || !!appId && item.sender.id === appId);
    const header: Message = {
        id: item.message_id, chatId: item.chat_id, chatType: room.dmUser ? 'p2p' : 'group', senderId: item.sender.id,
        senderIsBot: bot, senderIsSelf: self, text: '', time: Number(item.create_time ?? 0), mentioned: false, resources: []
    };
    if (!readable(room, header)) return undefined;
    const event: RawMessageEvent = {
        sender: { sender_id: { open_id: item.sender.id }, sender_type: bot ? 'bot' : 'user' },
        message: {
            message_id: item.message_id, chat_id: item.chat_id, chat_type: header.chatType,
            message_type: item.msg_type ?? 'text', content: item.body?.content ?? '{}', create_time: item.create_time,
            parent_id: item.parent_id, root_id: item.root_id, thread_id: item.thread_id,
            mentions: item.mentions?.map(m => ({ key: m.key, id: { [m.id_type]: m.id }, name: m.name }))
        }
    };
    const normalized = await normalizeEvent(event, { botIdentity: { openId: botOpenId ?? '', name: '' }, stripBotMentions: false });
    return { ...normalize(normalized), senderName: item.sender.sender_name, senderIsSelf: self };
}

/** Remove only this application's reaction; never the first arbitrary app reaction. */
export function reactionHandler(channel: LarkChannel, appId: string): (id: string, state?: ReceiptState, previouslyApplied?: ReceiptState) => Promise<void> {
    const processingIds = new Map<string, string>();
    const removeOwned = async (id: string, emoji: 'OnIt' | 'Done'): Promise<void> => {
        // The app must remove by reaction_id, never by emoji alone: other
        // applications and human participants may have the same reaction.
        let pageToken: string | undefined;
        for (let page = 0; page < 10; page++) {
            const response = await channel.rawClient.im.v1.messageReaction.list({ path: { message_id: id }, params: {
                reaction_type: emoji, page_size: 50, page_token: pageToken
            } });
            if (response.code !== undefined && response.code !== 0) throw new Error(`Reaction API error ${response.code}`);
            for (const item of response.data?.items ?? []) {
                const operator = item.operator;
                if (item.reaction_id && operator?.operator_type === 'app' &&
                    (operator.operator_id === appId || !!channel.botIdentity?.openId && operator.operator_id === channel.botIdentity.openId))
                    await channel.removeReaction(id, item.reaction_id);
            }
            if (!response.data?.has_more) break;
            pageToken = response.data.page_token;
            if (!pageToken || page === 9) throw new Error('Reaction cleanup page limit');
        }
    };
    return async (id, state = 'processing', previouslyApplied) => {
        if (state === 'processing') {
            processingIds.set(id, await channel.addReaction(id, 'OnIt'));
            return;
        }
        const known = processingIds.get(id);
        if (known) {
            await channel.removeReaction(id, known);
            processingIds.delete(id);
        } else if (previouslyApplied !== 'done') {
            // Recover a processing emoji after restart or an ambiguous add.
            await removeOwned(id, 'OnIt');
        }
        if (state === 'clear' && previouslyApplied === 'done') {
            // Done may have been accepted while the durable desired state was
            // changing to clear. Reconcile only this application's completed
            // reaction; an in-flight Done is serialized before this call.
            await removeOwned(id, 'Done');
        }
        if (state === 'done') await channel.addReaction(id, 'Done');
    };
}
