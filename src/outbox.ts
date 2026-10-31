import { randomUUID } from 'node:crypto';
import type { Room, Transport } from './types.ts';
import { Store } from './store.ts';
import { displayCard } from './approvals.ts';
export interface Outgoing {
    id: string;
    room: string;
    session: string;
    text: string;
    target?: string;
    replyTo?: string;
    threadId?: string;
    success: boolean;
    state: string;
}
export class Outbox {
    readonly store: Store;
    readonly transport: Transport;
    private active = new Set<string>();
    constructor(store: Store, transport: Transport) { this.store = store; this.transport = transport; }
    save(room: Room, session: string, text: string, success: boolean, target?: string, replyTo?: string, threadId?: string): string {
        const id = randomUUID();
        this.store.db.prepare('INSERT INTO outbox(id,room,session,text,target,reply_to,thread_id,success,state,at) VALUES(?,?,?,?,?,?,?,?,?,?)').run(id, room.key, session, text, target ?? null, replyTo ?? null, threadId ?? null, success ? 1 : 0, 'pending', Date.now());
        return id;
    }
    private get(id: string): Outgoing | undefined {
        const r = this.store.db.prepare('SELECT * FROM outbox WHERE id=?').get(id);
        if (!r)
            return;
        return { id: r.id as string, room: r.room as string, session: r.session as string, text: r.text as string, target: r.target as string | undefined, replyTo: r.reply_to as string | undefined, threadId: r.thread_id as string | undefined, success: !!r.success, state: r.state as string };
    }
    async deliver(id: string, room: Room, explicit = false): Promise<boolean> {
        if (this.active.has(id))
            return false;
        const row = this.get(id);
        if (!row || row.room !== room.key || row.state === 'done')
            return false;
        // A failed create might already have succeeded remotely. Only a human can authorize a possible duplicate.
        if (row.state === 'sending' || (row.state === 'uncertain' && !explicit))
            return false;
        const claimed = this.store.db.prepare("UPDATE outbox SET state='sending' WHERE id=? AND state=?").run(id, row.state).changes;
        if (!claimed)
            return false;
        this.active.add(id);
        try {
            const text = row.text.length > 24000 ? row.text.slice(0, 24000) + '\n\n（完整内容保留在服务输出记录；长回复附文件。）' : row.text;
            const card = displayCard(row.success ? '回复' : '执行失败', text || '本轮已结束。');
            let target = row.target;
            if (target)
                await this.transport.update(target, card);
            else
                target = await this.transport.card(room, card, row.replyTo, row.threadId);
            this.store.db.prepare("UPDATE outbox SET state='done',target=? WHERE id=?").run(target, id);
            this.store.recordOutput(target, room.key, row.session);
            return true;
        }
        catch {
            this.store.db.prepare('UPDATE outbox SET state=? WHERE id=?').run(row.target ? 'pending' : 'uncertain', id);
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
    }[] { return this.store.db.prepare("SELECT id,state FROM outbox WHERE room=? AND state!='done' ORDER BY at DESC LIMIT 20").all(room).map(r => ({ id: r.id as string, state: r.state as string })); }
}
