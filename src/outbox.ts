import { randomUUID } from 'node:crypto';
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
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
    fileName?: string;
    filePath?: string;
    fileTarget?: string;
    fileState: 'none' | 'pending' | 'sending' | 'uncertain' | 'done';
}
export class Outbox {
    readonly store: Store;
    readonly transport: Transport;
    private active = new Set<string>();
    readonly receipts?: Receipts;
    readonly allowed?: (row: Outgoing, room: Room) => boolean;
    constructor(store: Store, transport: Transport, receipts?: Receipts, allowed?: (row: Outgoing, room: Room) => boolean) { this.store = store; this.transport = transport; this.receipts = receipts; this.allowed = allowed; }
    save(room: Room, session: string, text: string, success: boolean, target?: string, replyTo?: string, threadId?: string, card?: object, origins: Origin[] = [], file?: { name: string; directory: string }): string {
        const id = randomUUID();
        // The full answer is the durable source. A local export can be regenerated after a restart.
        this.store.db.prepare('INSERT INTO outbox(id,room,session,text,target,reply_to,thread_id,success,state,at,card,origins,file_name,file_path,file_state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)').run(
            id, room.key, session, text, target ?? null, replyTo ?? null, threadId ?? null, success ? 1 : 0, 'pending', Date.now(),
            card ? JSON.stringify(card) : null, JSON.stringify(origins), file?.name ?? null, file ? join(file.directory, `${id}.md`) : null, file ? 'pending' : 'none');
        return id;
    }
    private get(id: string): Outgoing | undefined {
        const r = this.store.db.prepare('SELECT * FROM outbox WHERE id=?').get(id);
        if (!r) return undefined;
        return { id: String(r.id), room: String(r.room), session: String(r.session), text: String(r.text),
            card: r.card ? JSON.parse(String(r.card)) : undefined, origins: JSON.parse(String(r.origins ?? '[]')),
            target: r.target as string | undefined, replyTo: r.reply_to as string | undefined, threadId: r.thread_id as string | undefined,
            success: !!r.success, state: String(r.state), fileName: r.file_name as string | undefined,
            filePath: r.file_path as string | undefined, fileTarget: r.file_target as string | undefined,
            fileState: (r.file_state ?? 'none') as Outgoing['fileState'] };
    }
    private permitted(row: Outgoing, room: Room): boolean {
        if (!this.allowed || this.allowed(row, room)) return true;
        this.store.db.prepare("UPDATE outbox SET state='cancelled',text='',card=NULL WHERE id=?").run(row.id);
        this.receipts?.settle(room.key, row.session, row.origins, false);
        return false;
    }
    private sending(id: string, room: Room): boolean {
        const row = this.get(id);
        return !!row && row.state === 'sending' && this.permitted(row, room);
    }
    async deliver(id: string, room: Room, explicit = false): Promise<boolean> {
        if (this.active.has(id)) return false;
        const row = this.get(id);
        if (!row || row.room !== room.key || row.state === 'done' || row.state === 'cancelled') return false;
        if (!this.permitted(row, room)) return false;
        // Unknown creates (card or file) require an explicit decision about possible duplication.
        if (!['pending', 'uncertain'].includes(row.state) || (row.state === 'uncertain' && !explicit) ||
            (row.fileState === 'uncertain' && !explicit)) return false;
        const claimed = this.store.db.prepare("UPDATE outbox SET state='sending',delivery_error=NULL WHERE id=? AND state=?").run(id, row.state).changes;
        if (!claimed) return false;
        this.active.add(id);
        let target = row.target;
        let phase: 'card' | 'file_prepare' | 'file_send' = 'card';
        try {
            const text = row.text.length > 24000 ? row.text.slice(0, 23800) + '\n\n（完整答案另附文件。）' : row.text;
            const card = row.card ?? displayCard(row.success ? '回复' : '执行失败', text || '本轮已结束。');
            if (target) await this.transport.update(target, card);
            else target = await this.transport.card(room, card, row.replyTo, row.threadId);
            const saved = this.store.db.prepare("UPDATE outbox SET target=? WHERE id=? AND state='sending'").run(target, id).changes;
            if (!saved || !this.sending(id, room)) return false;
            this.store.recordOutput(target, room.key, row.session);
            if (row.fileName && row.fileState !== 'done') {
                phase = 'file_prepare';
                if (!row.filePath) throw new Error('Missing export destination');
                await mkdir(dirname(row.filePath), { recursive: true, mode: 0o700 });
                await writeFile(row.filePath, row.text, { mode: 0o600 });
                if (!this.sending(id, room)) return false;
                this.store.db.prepare("UPDATE outbox SET file_state='sending' WHERE id=? AND state='sending'").run(id);
                phase = 'file_send';
                const fileTarget = await this.transport.file(room, row.filePath, row.fileName);
                const fileSaved = this.store.db.prepare("UPDATE outbox SET file_target=?,file_state='done' WHERE id=? AND state='sending'").run(fileTarget, id).changes;
                if (!fileSaved || !this.sending(id, room)) return false;
                this.store.recordOutput(fileTarget, room.key, row.session);
            }
            if (!this.sending(id, room)) return false;
            const completed = this.store.db.prepare("UPDATE outbox SET state='done',delivery_error=NULL WHERE id=? AND state='sending'").run(id).changes;
            if (!completed) return false;
            // The shortened card alone is not full delivery. Required attachments must also succeed.
            this.receipts?.settle(room.key, row.session, row.origins, row.success);
            return true;
        } catch {
            const state = phase === 'file_send' || !target ? 'uncertain' : 'pending';
            const reason = phase === 'file_send' ? 'Full-answer file delivery is unconfirmed; explicit retry may duplicate a previously accepted message.'
                : phase === 'file_prepare' ? 'Cannot prepare the full-answer file; check the data directory permissions and capacity.'
                : 'Reply card delivery is unconfirmed.';
            const changed = this.store.db.prepare("UPDATE outbox SET state=?,file_state=CASE WHEN file_state='sending' THEN 'uncertain' ELSE file_state END,delivery_error=? WHERE id=? AND state='sending'").run(state, reason, id).changes;
            this.store.audit(room.key, 'output_delivery_failed', { id, phase, knownTarget: !!target });
            if (changed && target && phase !== 'card') await this.pendingFileNotice(row, room, target, reason);
            return false;
        } finally { this.active.delete(id); }
    }
    private async pendingFileNotice(row: Outgoing, room: Room, target: string, reason: string): Promise<void> {
        const current = this.get(row.id);
        if (!current || !['pending', 'uncertain'].includes(current.state) || !this.permitted(current, room)) return;
        const card = structuredClone(row.card ?? displayCard('回复', row.text.slice(0, 23800))) as {
            header?: { title?: object }; body?: { elements?: object[] };
        };
        card.header = { ...card.header, title: { tag: 'plain_text', content: '回复尚未完整送达' } };
        card.body = { ...card.body, elements: [...(card.body?.elements ?? []), { tag: 'markdown',
            content: `完整文件尚未确认送达；未标记完成。可用 /outputs 查看状态，或 /retry-output ${row.id} 明确补发（可能重复已送达的文件）。` }] };
        try { await this.transport.update(target, card); }
        catch { this.store.audit(room.key, 'delivery_notice_pending', { id: row.id, reason }); }
    }
    retryable(room: string): string[] { return this.store.db.prepare("SELECT id FROM outbox WHERE room=? AND state='pending' AND target IS NOT NULL AND file_state!='uncertain' ORDER BY at LIMIT 10").all(room).map(r => String(r.id)); }
    pending(room: string): { id: string; state: string; stage: string; detail?: string }[] {
        return this.store.db.prepare("SELECT id,state,file_state,delivery_error FROM outbox WHERE room=? AND state NOT IN ('done','cancelled') ORDER BY at DESC LIMIT 20").all(room)
            .map(r => ({ id: String(r.id), state: String(r.state), stage: r.file_state !== 'none' && r.file_state !== 'done' ? 'file' : 'reply', ...(r.delivery_error ? { detail: String(r.delivery_error) } : {}) }));
    }
}
