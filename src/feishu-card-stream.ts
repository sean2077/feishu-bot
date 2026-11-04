import type { LarkChannel } from '@larksuite/channel';
import type { Store } from './store.ts';
import type { Room } from './types.ts';

type CardObject = Record<string, unknown>;
function object(value: unknown): value is CardObject { return !!value && typeof value === 'object' && !Array.isArray(value); }
function texts(value: unknown, result = new Map<string, string>()): Map<string, string> {
    if (Array.isArray(value)) for (const item of value) texts(item, result);
    else if (object(value)) {
        if (value.tag === 'markdown' && (value.element_id === 'reply_answer' || value.element_id === 'reply_thinking' || value.element_id === 'reply_status') && typeof value.content === 'string')
            result.set(value.element_id, value.content);
        for (const item of Object.values(value)) texts(item, result);
    }
    return result;
}
function native(card: unknown): boolean {
    return object(card) && card.schema === '2.0' && object(card.config) && typeof card.config.streaming_mode === 'boolean' && texts(card).has('reply_answer');
}
function structure(value: unknown): unknown {
    if (Array.isArray(value)) return value.map(structure);
    if (!object(value)) return value;
    const text = value.tag === 'markdown' && (value.element_id === 'reply_answer' || value.element_id === 'reply_thinking' || value.element_id === 'reply_status');
    return Object.fromEntries(Object.keys(value).sort().filter(key => !text || key !== 'content').map(key => [key, structure(value[key])]));
}
function checked(response: { code?: number }): void {
    if (response.code !== 0) throw new Error(`CardKit request failed (code ${response.code ?? 'unknown'}).`);
}
function permissionDenied(value: unknown): boolean {
    if (!object(value)) return false;
    return value.code === 99991672 || value.status === 403 ||
        (object(value.response) && (value.response.status === 403 || (object(value.response.data) && value.response.data.code === 99991672)));
}
function title(card: object): unknown {
    const value = card as CardObject;
    return object(value.header) && object(value.header.title) ? value.header.title.content : undefined;
}
function closedApproval(card: object): boolean {
    const value = card as CardObject;
    return (title(card) === '已确认' || title(card) === '请求已结束') && object(value.body) &&
        Array.isArray(value.body.elements) && value.body.elements.every(element => object(element) && element.tag === 'markdown' && typeof element.content === 'string' && Object.keys(element).every(key => key === 'tag' || key === 'content')) &&
        Object.keys(value).every(key => ['schema', 'config', 'header', 'body'].includes(key));
}

/** A known message always keeps its original delivery identity, including after restart. */
export function createReplyCardStream(channel: Pick<LarkChannel, 'rawClient' | 'send' | 'updateCard'>, store: Store,
    log: (event: string, detail: object) => void): {
        create(room: Room, card: object, replyTo?: string, threadId?: string): Promise<string>;
        update(messageId: string, card: object): Promise<void>;
    } {
    const pending = new Map<string, Promise<void>>();
    // Only this process's positively identified plain approval messages may get
    // a cosmetic disabling update after shutdown. Never infer this from a path,
    // caller-supplied message ID, or failed native-card creation.
    const plainApprovals = new Set<string>();
    const api = channel.rawClient.cardkit.v1;
    const requireOpen = () => { if (!store.isOpen) throw new Error('Card transport is closed.'); };
    const sequence = (message: string): number => {
        requireOpen();
        const row = store.db.prepare('UPDATE feishu_cards SET sequence=sequence+1 WHERE message=? RETURNING sequence').get(message);
        if (!row) throw new Error('CardKit delivery mapping is missing.');
        return Number(row.sequence);
    };
    const update = async (message: string, card: object) => {
        if (!store.isOpen && plainApprovals.has(message) && closedApproval(card)) {
            await channel.updateCard(message, card);
            plainApprovals.delete(message);
            return;
        }
        requireOpen();
        const saved = store.db.prepare('SELECT card,snapshot FROM feishu_cards WHERE message=?').get(message);
        if (!saved) {
            await channel.updateCard(message, card);
            if (closedApproval(card)) plainApprovals.delete(message);
            return;
        }
        const previous = JSON.parse(String(saved.snapshot));
        const path = { card_id: String(saved.card) };
        const json = JSON.stringify(card);
        if (json === saved.snapshot) return;
        const previousStreaming = object(previous) && object(previous.config) && previous.config.streaming_mode === true;
        if (!previousStreaming || !native(card) || JSON.stringify(structure(previous)) !== JSON.stringify(structure(card))) {
            const seq = sequence(message);
            checked(await api.card.update({ path, data: { card: { type: 'card_json', data: json }, sequence: seq, uuid: `u_${path.card_id}_${seq}` } }));
        } else {
            const oldTexts = texts(previous);
            for (const [element_id, content] of texts(card)) if (oldTexts.get(element_id) !== content) {
                requireOpen();
                const seq = sequence(message);
                checked(await api.cardElement.content({ path: { ...path, element_id }, data: { content, sequence: seq, uuid: `c_${path.card_id}_${seq}` } }));
            }
        }
        if (!store.isOpen) return;
        store.db.prepare('UPDATE feishu_cards SET snapshot=? WHERE message=?').run(json, message);
    };
    return {
        async create(room, card, replyTo, threadId) {
            requireOpen();
            const options = { replyTo, replyInThread: !!threadId };
            if (!native(card)) {
                const sent = await channel.send(room.chatId, { card }, options);
                if (title(card) === '需要补充信息' || title(card) === '需要维护者批准') plainApprovals.add(sent.messageId);
                return sent.messageId;
            }
            let cardId: string;
            try {
                const response = await api.card.create({ data: { type: 'card_json', data: JSON.stringify(card) } });
                if (permissionDenied(response)) throw response;
                checked(response);
                if (!response.data?.card_id) throw new Error('CardKit creation returned no card identity.');
                cardId = response.data.card_id;
            } catch (error) {
                if (!permissionDenied(error)) throw error;
                requireOpen();
                log('card_stream_unavailable', { reason: 'cardkit_permission_denied', fallback: 'inline_card' });
                return (await channel.send(room.chatId, { card }, options)).messageId;
            }
            // A send failure may mean the message exists: never create a replacement.
            requireOpen();
            const sent = await channel.send(room.chatId, { card: { type: 'card', data: { card_id: cardId } } }, options);
            if (!store.isOpen) return sent.messageId;
            store.db.prepare('INSERT INTO feishu_cards(message,card,room,sequence,snapshot) VALUES(?,?,?,0,?)')
                .run(sent.messageId, cardId, room.key, JSON.stringify(card));
            return sent.messageId;
        },
        async update(messageId, card) {
            const next = (pending.get(messageId) ?? Promise.resolve()).catch(() => {}).then(() => update(messageId, card));
            pending.set(messageId, next);
            try { await next; }
            finally { if (pending.get(messageId) === next) pending.delete(messageId); }
        }
    };
}
