import { randomUUID } from 'node:crypto';
import type { Room, Transport, Origin } from './types.ts';
import { Store } from './store.ts';
import { displayCard } from './approvals.ts';
import type { Receipts } from './receipts.ts';
export interface Outgoing {
    id: string;
    room: string;
    session: string;
    text: string;
    target?: string;
    card?: object;
    origins: Origin[];
    replyTo?: string;
    threadId?: string;
    success: boolean;
    state: string;
}
export class Outbox {
    readonly store: Store;
    readonly transport: Transport;
    private active = new Set<string>();
    readonly receipts?: Receipts;
    readonly allowed?: (row: Outgoing, room: Room) => boolean;
    constructor(store: Store, transport: Transport, receipts?: Receipts, allowed?: (row: Outgoing, room: Room) => boolean) { this.store = store; this.transport = transport; this.receipts = receipts; this.allowed = allowed; }
    save(room: Room, session: string, text: string, success: boolean, target?: string, replyTo?: string, threadId?: string, card?: object, origins: Origin[] = []): string {
        const id = randomUUID();
        this.store.db.prepare('INSERT INTO outbox(id,room,session,text,target,reply_to,thread_id,success,state,at,card,origins) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)').run(id, room.key, session, text, target ?? null, replyTo ?? null, threadId ?? null, success ? 1 : 0, 'pending', Date.now(), card ? JSON.stringify(card) : null, JSON.stringify(origins));
        return id;
    }
    private get(id: string): Outgoing | undefined {
        const r = this.store.db.prepare('SELECT * FROM outbox WHERE id=?').get(id);
        if (!r)
            return;
        return { id: r.id as string, room: r.room as string, session: r.session as string, text: r.text as string, card: r.card ? JSON.parse(String(r.card)) : undefined, origins: JSON.parse(String(r.origins ?? '[]')), target: r.target as string | undefined, replyTo: r.reply_to as string | undefined, threadId: r.thread_id as string | undefined, success: !!r.success, state: r.state as string };
    }
    async deliver(id: string, room: Room, explicit = false): Promise<boolean> {
        if (this.active.has(id))
            return false;
        const row = this.get(id);
        if (!row || row.room !== room.key || row.state === 'done' || row.state === 'cancelled')
            return false;
        if (this.allowed && !this.allowed(row, room)) {
            this.store.db.prepare("UPDATE outbox SET state='cancelled',text='',card=NULL WHERE id=?").run(id);
            this.receipts?.settle(room.key, row.session, row.origins, false);
            return false;
        }
        // A failed create might already have succeeded remotely. Only a human can authorize a possible duplicate.
        if (!['pending', 'uncertain'].includes(row.state) || (row.state === 'uncertain' && !explicit))
            return false;
        const claimed = this.store.db.prepare("UPDATE outbox SET state='sending' WHERE id=? AND state=?").run(id, row.state).changes;
        if (!claimed)
            return false;
        this.active.add(id);
        try {
            const text = row.text.length > 24000 ? row.text.slice(0, 24000) + '\n\n（完整内容保留在服务输出记录；长回复附文件。）' : row.text;
            const card = row.card ?? displayCard(row.success ? '回复' : '执行失败', text || '本轮已结束。');
            let target = row.target;
            if (target)
                await this.transport.update(target, card);
            else
                target = await this.transport.card(room, card, row.replyTo, row.threadId);
            const completed = this.store.db.prepare("UPDATE outbox SET state='done',target=? WHERE id=? AND state='sending'").run(target, id).changes;
            if (!completed) return false; // Stop/cancel may have won while the HTTP request was in flight.
            this.receipts?.settle(room.key, row.session, row.origins, row.success);
            this.store.recordOutput(target, room.key, row.session);
            return true;
        }
        catch {
            this.store.db.prepare("UPDATE outbox SET state=? WHERE id=? AND state='sending'").run(row.target ? 'pending' : 'uncertain', id);
            this.store.audit(room.key, 'output_delivery_failed', { id, knownTarget: !!row.target });
            return false;
        }
        finally {
            this.active.delete(id);
        }
    }
    retryable(room: string): string[] { return this.store.db.prepare("SELECT id FROM outbox WHERE room=? AND state='pending' AND target IS NOT NULL ORDER BY at LIMIT 10").all(room).map(r => r.id as string); }
    pending(room: string): {
        id: string;
        state: string;
    }[] { return this.store.db.prepare("SELECT id,state FROM outbox WHERE room=? AND state NOT IN ('done','cancelled') ORDER BY at DESC LIMIT 20").all(room).map(r => ({ id: r.id as string, state: r.state as string })); }
}
