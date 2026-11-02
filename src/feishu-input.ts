import type { NormalizedMessage } from '@larksuite/channel';
import type { Message, Resource } from './types.ts';

type RecordValue = Record<string, unknown>;
const record = (value: unknown): value is RecordValue => !!value && typeof value === 'object' && !Array.isArray(value);
const nonempty = (value: unknown): value is string => typeof value === 'string' && value.length > 0 && value.length <= 4096 && !/[\r\n\0]/.test(value);
const invalid = '消息内容或资源信息缺失，无法完整解析。请重新发送原文或附件。';
const unsupported = '这种消息内容尚不支持完整解析。请改发普通文本、图片或文件。';
const forwarded = '合并转发不会自动展开。请在当前聊天发送获准使用的原始消息或附件。';

/** Validate completeness, not render content: all supported rendering stays with the pinned SDK. */
function contentIssue(msg: NormalizedMessage, resources: Resource[], malformedResource: boolean): string | undefined {
    if (msg.rawContentType === 'merge_forward') return forwarded;
    if (malformedResource || resources.length > 12) return invalid;
    if (!['text', 'post', 'image', 'file', 'audio', 'video', 'media', 'interactive'].includes(msg.rawContentType)) return unsupported;
    if (['image', 'file', 'audio', 'video', 'media'].includes(msg.rawContentType) && !resources.length) return invalid;
    // Raw content is used transiently to detect loss, and is never copied into Message.
    const raw = msg.raw;
    if (!record(raw) || !record(raw.message) || typeof raw.message.content !== 'string') return undefined;
    if (raw.message.content.length > 1024 * 1024) return invalid;
    let parsed: unknown;
    try { parsed = JSON.parse(raw.message.content); } catch { return invalid; }
    if (!record(parsed)) return invalid;
    if (msg.rawContentType === 'text') return typeof parsed.text === 'string' ? undefined : invalid;
    if (msg.rawContentType === 'image') return nonempty(parsed.image_key) ? undefined : invalid;
    if (['file', 'audio', 'video', 'media'].includes(msg.rawContentType)) return nonempty(parsed.file_key) ? undefined : invalid;
    if (msg.rawContentType !== 'post') return undefined;

    let body: RecordValue | undefined;
    if ('title' in parsed || 'content' in parsed) body = parsed;
    else {
        // Same locale precedence as Channel SDK 0.7.1; do not concatenate translated copies.
        for (const locale of ['zh_cn', 'en_us', 'ja_jp']) if (record(parsed[locale])) { body = parsed[locale]; break; }
        if (!body) { const first = parsed[Object.keys(parsed)[0] ?? '']; if (record(first)) body = first; }
    }
    const expected: { key: string; kind: Resource['kind'] }[] = [];
    if (parsed.files !== undefined) {
        if (!Array.isArray(parsed.files) || parsed.files.length > 12) return invalid;
        for (const entry of parsed.files) {
            if (!record(entry) || !nonempty(entry.file_key) || (entry.file_name !== undefined && typeof entry.file_name !== 'string')) return invalid;
            if (entry.is_folder === true) return '文件夹引用不是可下载附件，请发送需要处理的具体文件。';
            expected.push({ key: entry.file_key, kind: 'file' });
        }
    }
    if (!body && !expected.length) return invalid;
    if (body?.title !== undefined && typeof body.title !== 'string') return invalid;
    const paragraphs = Array.isArray(body?.content_v2) && body.content_v2.length ? body.content_v2 : body?.content;
    if (paragraphs !== undefined && !Array.isArray(paragraphs)) return invalid;
    let elements = 0;
    for (const paragraph of (paragraphs ?? []) as unknown[]) {
        if (!Array.isArray(paragraph)) return invalid;
        for (const element of paragraph) {
            if (++elements > 4096 || !record(element)) return invalid;
            if (!['text', 'a', 'at', 'img', 'media', 'code_block', 'hr', 'md'].includes(String(element.tag))) return unsupported;
            for (const key of ['text', 'href', 'user_id', 'user_name', 'language'])
                if (element[key] !== undefined && typeof element[key] !== 'string') return invalid;
            if (element.style !== undefined && (!Array.isArray(element.style) || element.style.some(s => typeof s !== 'string'))) return invalid;
            if (element.tag === 'img' || element.tag === 'media') {
                const key = element.tag === 'img' ? element.image_key : element.file_key;
                if (!nonempty(key)) return invalid;
                expected.push({ key, kind: element.tag === 'img' ? 'image' : 'file' });
            }
        }
    }
    if (expected.some(e => !resources.some(r => r.key === e.key && r.kind === e.kind))) return invalid;
    if (!msg.content?.trim() || (!elements && !expected.length && !body?.title)) return invalid;
    return undefined;
}

/** SDK → application boundary. Never retain raw payloads or infer an @ from body text. */
export function normalizeFeishuMessage(msg: NormalizedMessage): Message {
    let malformedResource = false;
    const resources: Resource[] = [];
    for (const item of msg.resources ?? []) {
        if (!item || !['image', 'file', 'audio', 'video'].includes(item.type) || !nonempty(item.fileKey) ||
            (item.fileName !== undefined && typeof item.fileName !== 'string')) { malformedResource = true; continue; }
        resources.push({ key: item.fileKey, kind: item.type === 'image' ? 'image' : 'file',
            name: item.fileName ?? `${item.type}-${item.fileKey.slice(-12)}` });
    }
    const inputIssue = contentIssue(msg, resources, malformedResource);
    return { id: msg.messageId, chatId: msg.chatId, chatType: msg.chatType, senderId: msg.senderId, senderName: msg.senderName,
        senderIsBot: msg.senderType !== 'user', text: inputIssue ? `[内容未完整解析：${inputIssue}]` : msg.content,
        time: msg.createTime, mentioned: msg.mentionedBot, replyTo: msg.replyToMessageId, threadId: msg.threadId,
        resources: inputIssue ? [] : resources, inputIssue,
        imageOnly: !inputIssue && msg.rawContentType === 'image' && resources.length > 0 && resources.every(r => r.kind === 'image') };
}
