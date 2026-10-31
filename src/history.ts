import { randomBytes, createHmac, timingSafeEqual } from 'node:crypto';
import type { Args, Config, HistoryQuery, Message, Room, Transport } from './types.ts';
import { Store } from './store.ts';
import { readable, roomFromKey } from './policy.ts';

/** One bounded API page on explicit tool demand. No background archive or prefetch. */
export class ChatHistory {
    private key = randomBytes(32);
    readonly config: Config;
    readonly store: Store;
    readonly transport: Transport;
    constructor(config: Config, store: Store, transport: Transport) { this.config = config; this.store = store; this.transport = transport; }

    async read(room: Room, sessionId: string, input: Args, signal?: AbortSignal): Promise<Args> {
        const current = roomFromKey(this.config, room.key);
        if (!current || signal?.aborted) throw new Error('History scope is no longer available');
        const fields = new Set(['limit', 'startTime', 'endTime', 'cursor', 'threadId']);
        if (Object.keys(input).some(key => !fields.has(key))) throw new Error('Unsupported history parameter');
        let query: HistoryQuery;
        if (input.cursor !== undefined) {
            if (Object.keys(input).length !== 1 || typeof input.cursor !== 'string' || input.cursor.length > 16000)
                throw new Error('Use the returned cursor alone for the next page');
            query = this.openCursor(input.cursor, current.key, sessionId);
        } else {
            const limit = input.limit === undefined ? 20 : input.limit;
            if (!Number.isSafeInteger(limit) || Number(limit) < 1 || Number(limit) > 50) throw new Error('limit must be 1..50');
            for (const name of ['startTime', 'endTime'])
                if (input[name] !== undefined && (!Number.isSafeInteger(input[name]) || Number(input[name]) < 0))
                    throw new Error(`${name} must be a Unix timestamp in seconds`);
            const endTime = input.endTime === undefined ? Math.floor(Date.now() / 1000) : Number(input.endTime);
            const startTime = input.startTime === undefined ? undefined : Number(input.startTime);
            if (startTime !== undefined && startTime > endTime) throw new Error('Invalid history time range');
            if (input.threadId !== undefined && (typeof input.threadId !== 'string' || input.threadId.length > 200))
                throw new Error('Invalid thread ID');
            query = { limit: Number(limit), startTime, endTime, threadId: input.threadId as string | undefined };
        }
        if (query.threadId && !this.store.hasContextThread(current.key, query.threadId))
            throw new Error('Thread must first be observed in this conversation');
        const page = await this.transport.history(current, query);
        const latest = roomFromKey(this.config, current.key);
        if (signal?.aborted || !latest) throw new Error('History request cancelled');
        // Filter BEFORE remembering metadata or exposing text. Never fill a page by unbounded scanning.
        let remaining = Math.min(this.config.limits.textChars * 2, 40000);
        const messages: Message[] = [];
        for (const message of page.messages.slice(0, query.limit)) {
            if (!readable(latest, message)) continue;
            const text = message.text.slice(0, Math.min(this.config.limits.textChars, remaining));
            remaining -= text.length;
            messages.push({ ...message, text, resources: message.resources.slice(0, 12) });
        }
        this.store.rememberContext(current.key, messages);
        const cursor = page.nextPageToken
            ? this.sealCursor(current.key, sessionId, { ...query, pageToken: page.nextPageToken }) : undefined;
        return { messages, nextCursor: cursor, hasMore: !!cursor,
            note: 'Only authorized authors and this bot are returned. This is one API page, not the full history. Times are milliseconds in messages; query times use seconds.' };
    }
    private sealCursor(room: string, session: string, query: HistoryQuery): string {
        const payload = Buffer.from(JSON.stringify({ room, session, query, expires: Date.now() + 15 * 60000 })).toString('base64url');
        return payload + '.' + createHmac('sha256', this.key).update(payload).digest('base64url');
    }
    private openCursor(cursor: string, room: string, session: string): HistoryQuery {
        try {
            const [payload, signature, extra] = cursor.split('.');
            if (!payload || !signature || extra) throw new Error();
            const expected = createHmac('sha256', this.key).update(payload).digest();
            const supplied = Buffer.from(signature, 'base64url');
            if (expected.length !== supplied.length || !timingSafeEqual(expected, supplied)) throw new Error();
            const data = JSON.parse(Buffer.from(payload, 'base64url').toString());
            if (data.room !== room || data.session !== session || data.expires < Date.now()) throw new Error();
            return data.query as HistoryQuery;
        } catch { throw new Error('History cursor expired or belongs to another conversation'); }
    }
}
