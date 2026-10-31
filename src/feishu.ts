import { createLarkChannel, type NormalizedMessage } from '@larksuite/channel';
import { readFile, stat } from 'node:fs/promises';
import type { Config, Message, Resource, Transport } from './types.ts';
export function normalize(msg: NormalizedMessage): Message {
    return { id: msg.messageId, chatId: msg.chatId, chatType: msg.chatType, senderId: msg.senderId, senderName: msg.senderName,
        senderIsBot: msg.senderType !== 'user', text: msg.content, time: msg.createTime,
        mentioned: msg.mentionedBot, replyTo: msg.replyToMessageId, threadId: msg.threadId,
        resources: msg.resources.filter(r => ['image', 'file', 'audio', 'video'].includes(r.type)).map(r => ({ key: r.fileKey, kind: r.type === 'image' ? 'image' : 'file', name: r.fileName ?? `${r.type}-${r.fileKey.slice(-12)}` })) };
}
export function feishuTransport(config: Config, log: (event: string, detail: object) => void): {
    transport: Transport;
    channel: ReturnType<typeof createLarkChannel>;
} {
    const secret = process.env[config.appSecretEnv];
    if (!secret)
        throw new Error(`missing Feishu credential ${config.appSecretEnv}`);
    const channel = createLarkChannel({ appId: config.appId, appSecret: secret, resolveSenderNames: true, includeRawEvent: false,
        respectProxyEnv: true, httpTimeoutMs: 20000,
        policy: { requireMention: false, groupAllowlist: config.groups.length ? config.groups.map(g => g.chatId) : ['__disabled__'], dmMode: 'allowlist', dmAllowlist: config.dms.map(d => d.userId) },
        safety: { chatQueue: { enabled: true, mergeWhileBusy: false, cardActions: 'separate' }, batch: { text: { delayMs: 0, longDelayMs: 0, maxMessages: 1 }, media: { delayMs: 0, maxItems: 1 } } },
        logger: { debug: () => { }, trace: () => { }, info: () => { }, warn: () => log('feishu_warning', {}), error: () => log('feishu_sdk_error', {}) },
        // Do not blindly retry message creation after an ambiguous network failure.
        // Known-ID card updates are retried through the durable application outbox.
        outbound: { retry: { maxAttempts: 1, baseDelayMs: 500 }, ssrfGuard: true } });
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
        fetch: async (id) => { const msg = await channel.fetchMessage(id); return msg ? normalize(msg) : undefined; },
        receipt: async (id) => { await channel.addReaction(id, 'Get'); },
        file: async (room, path, name) => { if ((await stat(path)).size > config.limits.attachmentBytes)
            throw new Error('outbound file exceeds limit'); const bytes = await readFile(path); if (bytes.length > config.limits.attachmentBytes)
            throw new Error('outbound file exceeds limit'); return (await channel.send(room.chatId, { file: { source: bytes, fileName: name } })).messageId; }
    };
    return { transport, channel };
}
