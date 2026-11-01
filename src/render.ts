import { join } from 'node:path';
import type { Delivery, Origin, Room, Transport } from './types.ts';
import { Store } from './store.ts';
import { Outbox } from './outbox.ts';
import { replyCard } from './reply-card.ts';
import { hash } from './util.ts';

/** Network updates never block CC's event reader. The known message ID is retained on errors. */
export class Render {
    private blocks = new Map<string, { kind: 'text' | 'thinking'; text: string }>();
    private messageId?: string;
    private finalId?: string;
    private createUncertain = false;
    private timer?: ReturnType<typeof setTimeout>;
    private closed = false;
    private tail: Promise<void> = Promise.resolve();
    private last = '';
    private status = '处理中';
    private legacy = 0;
    replyTo?: string;
    threadId?: string;
    readonly room: Room;
    readonly session: string;
    readonly transport: Transport;
    readonly store: Store;
    readonly outbox: Outbox;
    readonly interval: number;
    readonly signal?: AbortSignal;
    readonly redactText: (text: string) => string;
    constructor(room: Room, session: string, transport: Transport,
        store: Store, outbox: Outbox, interval: number, anchor?: Delivery, signal?: AbortSignal, redactText: (text: string) => string = text => text) {
        this.room = room; this.session = session; this.transport = transport;
        this.store = store; this.outbox = outbox; this.interval = interval; this.signal = signal; this.redactText = redactText;
        this.replyTo = anchor?.message.id; this.threadId = anchor?.message.threadId;
    }
    bindOrigin(anchor?: Delivery): void {
        if (!this.messageId && anchor) { this.replyTo = anchor.message.id; this.threadId = anchor.message.threadId; }
    }
    setStatus(status: string): void { this.status = status; this.schedule(); }
    update(text: string, partial: boolean, kind: 'text' | 'thinking' = 'text', blockId?: string): void {
        if (this.closed || this.signal?.aborted) return;
        const key = blockId ?? `${kind}:legacy:${this.legacy}`;
        const previous = this.blocks.get(key);
        const limit = kind === 'thinking' ? 32000 : 60000;
        this.blocks.set(key, { kind, text: (partial ? (previous?.text ?? '') + text : text).slice(0, limit) });
        // Final assistant snapshots use the same block ID as their deltas, never append twice.
        if (!blockId && !partial) this.legacy++;
        if (this.blocks.size > 256) this.blocks.delete(this.blocks.keys().next().value!);
        this.schedule();
    }
    private content(kind: 'text' | 'thinking'): string {
        return this.redactText([...this.blocks.values()].filter(b => b.kind === kind).map(b => b.text).join('\n\n').slice(0, kind === 'thinking' ? 32000 : 500000));
    }
    private schedule(): void {
        if (this.closed || this.timer) return;
        this.timer = setTimeout(() => { this.timer = undefined; this.flush(); }, this.interval);
        this.timer.unref();
    }
    private flush(): void {
        this.tail = this.tail.then(async () => {
            if (this.signal?.aborted || !this.replyTo) return; // Buffer unattributed background content until its result supplies an origin.
            const card = replyCard(this.status, this.content('text'), this.content('thinking'));
            const snapshot = JSON.stringify(card);
            if (snapshot === this.last || (!this.messageId && this.createUncertain)) return;
            try {
                if (this.messageId) await this.transport.update(this.messageId, card);
                else {
                    this.messageId = await this.transport.card(this.room, card, this.replyTo, this.threadId);
                    this.store.recordOutput(this.messageId, this.room.key, this.session);
                    if (this.finalId) this.store.db.prepare("UPDATE outbox SET target=? WHERE id=? AND state='preparing'").run(this.messageId, this.finalId);
                }
                this.last = snapshot;
            } catch {
                if (!this.messageId) this.createUncertain = true;
                this.store.audit(this.room.key, 'preview_delivery_pending', { session: this.session });
            }
        }).catch(() => {});
    }
    async cancel(title = '已停止'): Promise<void> {
        this.closed = true; clearTimeout(this.timer);
        await this.tail;
        if (this.messageId) await this.transport.update(this.messageId,
            replyCard(title, '以下为中断前输出，不代表任务完成。\n\n' + this.content('text'), this.content('thinking'), true)).catch(() => {});
    }
    async finish(text: string, success: boolean, dataDir: string, origins: Origin[], title?: string, onPersisted?: () => void): Promise<void> {
        if (this.closed) return;
        this.closed = true; clearTimeout(this.timer);
        const answer = this.redactText(text || this.content('text'));
        const thinking = this.content('thinking');
        const body = answer.length > 24000 ? answer.slice(0, 23800) + '\n\n（完整答案另附文件。）' : answer;
        const card = replyCard(title ?? (success ? '回复' : '执行失败'), body, thinking, true);
        // Persist before awaiting slow preview I/O. Recovery will never lose the final answer or blindly recreate an uncertain preview.
        const file = answer.length > 24000 ? { name: 'reply.md', directory: join(dataDir, 'exports', hash(this.room.key).slice(0, 32)) } : undefined;
        // The final answer and its originating input's completion are one
        // durable transition. A failed insert must leave the input recoverable.
        const id = this.store.transaction(() => {
            const saved = this.outbox.save(this.room, this.session, answer, success, this.messageId, this.replyTo, this.threadId, card, origins, file);
            this.store.db.prepare("UPDATE outbox SET state='preparing' WHERE id=?").run(saved);
            onPersisted?.();
            return saved;
        });
        this.finalId = id;
        await this.tail;
        if (this.signal?.aborted) {
            this.store.db.prepare("UPDATE outbox SET state='cancelled' WHERE id=? AND state='preparing'").run(id);
            return;
        }
        const state = this.createUncertain && !this.messageId ? 'uncertain' : 'pending';
        const changed = this.store.db.prepare("UPDATE outbox SET target=?,state=? WHERE id=? AND state='preparing'").run(this.messageId ?? null, state, id).changes;
        if (!changed) return;
        await this.outbox.deliver(id, this.room);
    }
}
