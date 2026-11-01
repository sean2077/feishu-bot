import { randomUUID } from 'node:crypto';
import { mkdir, realpath, stat, unlink, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { inside } from './util.ts';
import type { Room, Transport, Origin } from './types.ts';
import { Store } from './store.ts';
import { displayCard } from './approvals.ts';
import type { Receipts } from './receipts.ts';
export interface Outgoing {
    id: string;
    kind: 'reply' | 'file';
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
    private retryAfter = new Map<string, { at: number; id: string }>();
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
    /** A file created by the agent is a first-class durable output, not a best-effort side effect. */
    saveFile(room: Room, session: string, path: string, name: string, origins: Origin[] = [], replyTo?: string): string {
        const id = randomUUID();
        // The caller snapshots the bytes into dataDir before inserting this record.
        this.store.db.prepare("INSERT INTO outbox(id,room,session,text,reply_to,success,state,at,origins,file_name,file_path,file_state,kind) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)").run(
            id, room.key, session, '', replyTo ?? null, 1, 'pending', Date.now(),
            JSON.stringify(origins), name, path, 'pending', 'file');
        return id;
    }
    /** Session-wide conservative barrier: do not claim success while any agent file is unconfirmed. */
    private unfinishedFiles(room: string, session: string): number {
        return Number(this.store.db.prepare("SELECT count(*) AS n FROM outbox WHERE room=? AND session=? AND kind='file' AND state NOT IN ('done','cancelled')").get(room, session)?.n ?? 0);
    }
    private get(id: string): Outgoing | undefined {
        const r = this.store.db.prepare('SELECT * FROM outbox WHERE id=?').get(id);
        if (!r) return undefined;
        return { id: String(r.id), kind: r.kind === 'file' ? 'file' : 'reply', room: String(r.room), session: String(r.session), text: String(r.text),
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
        // A timed-out file upload may already have succeeded; only an explicit retry can repeat it.
        if (!['pending', 'uncertain'].includes(row.state) || (row.state === 'uncertain' && !explicit) ||
            (row.fileState === 'uncertain' && !explicit)) return false;
        const claimed = this.store.db.prepare("UPDATE outbox SET state='sending',delivery_error=NULL WHERE id=? AND state=?").run(id, row.state).changes;
        if (!claimed) return false;
        this.active.add(id);
        let target = row.target;
        let phase: 'card' | 'file_prepare' | 'file_send' = row.kind === 'file' ? 'file_prepare' : 'card';
        try {
            if (row.kind === 'file') {
                if (row.fileState !== 'done') {
                    if (!row.filePath || !row.fileName) throw new Error('Missing file snapshot');
                    await stat(row.filePath);
                    if (!this.sending(id, room)) return false;
                    this.store.db.prepare("UPDATE outbox SET file_state='sending' WHERE id=? AND state='sending'").run(id);
                    phase = 'file_send';
                    const fileTarget = await this.transport.file(room, row.filePath, row.fileName);
                    const saved = this.store.db.prepare("UPDATE outbox SET file_target=?,file_state='done' WHERE id=? AND state='sending'").run(fileTarget, id).changes;
                    if (!saved || !this.sending(id, room)) return false;
                    this.store.recordOutput(fileTarget, room.key, row.session);
                } else {
                    if (!row.fileTarget) throw new Error('Missing recorded file receipt');
                    // A crash may occur after persisting the provider ID but before indexing it.
                    this.store.recordOutput(row.fileTarget, room.key, row.session);
                }
            } else {
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
                if (row.success && this.unfinishedFiles(room.key, row.session)) {
                    this.store.db.prepare("UPDATE outbox SET state='pending',delivery_error=? WHERE id=? AND state='sending'").run(
                        'An agent file has not been confirmed delivered; the reply is not complete.', id);
                    if (target) await this.pendingAttachmentNotice(row, room, target);
                    return false;
                }
            }
            if (!this.sending(id, room)) return false;
            const completed = this.store.db.prepare("UPDATE outbox SET state='done',delivery_error=NULL WHERE id=? AND state='sending'").run(id).changes;
            if (!completed) return false;
            // File jobs alone never mark a turn Done. Only a fully delivered final reply can do so.
            if (row.kind === 'reply') this.receipts?.settle(room.key, row.session, row.origins, row.success);
            return true;
        } catch {
            const state = phase === 'file_send' || (phase === 'card' && !target) ? 'uncertain' : 'pending';
            const reason = phase === 'file_send' ? row.kind === 'file'
                ? 'Agent file upload is unconfirmed; an explicit retry may duplicate a previously accepted file.'
                : 'Full-answer file delivery is unconfirmed; explicit retry may duplicate a previously accepted message.'
                : phase === 'file_prepare' ? 'Cannot read or prepare the file; check the data directory permissions and capacity.'
                : 'Reply card delivery is unconfirmed.';
            const changed = this.store.db.prepare("UPDATE outbox SET state=?,file_state=CASE WHEN file_state='sending' THEN 'uncertain' ELSE file_state END,delivery_error=? WHERE id=? AND state='sending'").run(state, reason, id).changes;
            this.store.audit(room.key, 'output_delivery_failed', { id, phase, kind: row.kind, knownTarget: !!target });
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
    private async pendingAttachmentNotice(row: Outgoing, room: Room, target: string): Promise<void> {
        const current = this.get(row.id);
        if (!current || current.state !== 'pending' || !this.permitted(current, room)) return;
        const card = structuredClone(row.card ?? displayCard('回复', row.text.slice(0, 23800))) as {
            header?: { title?: object }; body?: { elements?: object[] };
        };
        card.header = { ...card.header, title: { tag: 'plain_text', content: '回复附件尚未完整送达' } };
        card.body = { ...card.body, elements: [...(card.body?.elements ?? []), { tag: 'markdown',
            content: '主动发送的文件尚未确认送达；本轮未标记完成。可用 /outputs 查看待交付记录，必要时由维护者明确补发。' }] };
        try { await this.transport.update(target, card); }
        catch { this.store.audit(room.key, 'delivery_notice_pending', { id: row.id }); }
    }
    /** Prune terminal exports only. Unknown uploads retain their bytes for explicit recovery. */
    async pruneCompletedExports(now: number, exportsDir: string): Promise<void> {
        const root = resolve(exportsDir);
        const rows = this.store.db.prepare("SELECT id,file_path FROM outbox WHERE state IN ('done','cancelled') AND file_path IS NOT NULL AND at<? ORDER BY at LIMIT 100")
            .all(now - 7 * 86400000);
        for (const row of rows) {
            const id = String(row.id), storedPath = String(row.file_path), path = resolve(storedPath);
            if (path === root || !inside(root, path)) continue;
            try {
                // A replaced parent directory must not redirect cleanup outside managed exports.
                const [physicalRoot, parent] = await Promise.all([realpath(root), realpath(dirname(path))]);
                if (!inside(physicalRoot, parent)) continue;
                await unlink(path);
            } catch (error) {
                if ((error as NodeJS.ErrnoException).code !== 'ENOENT') continue;
            }
            // Keep a row if removal fails; never discard the only pointer to retained bytes.
            this.store.db.prepare("UPDATE outbox SET file_path=NULL WHERE id=? AND state IN ('done','cancelled') AND file_path=?")
                .run(id, storedPath);
        }
    }
    retryable(room: string): string[] {
        // A permanently failing oldest batch must not block newer known-ID
        // updates. Keep a separate bounded cursor per conversation and wrap.
        // Unknown external creates/uploads remain excluded from auto-retry.
        const eligible = "SELECT o.id,o.at FROM outbox o WHERE o.room=? AND o.state='pending' " +
            "AND o.file_state!='uncertain' AND (o.target IS NOT NULL OR (o.kind='file' AND o.file_state IN ('pending','done'))) " +
            "AND (o.kind='file' OR NOT EXISTS (SELECT 1 FROM outbox f WHERE f.room=o.room AND f.session=o.session " +
            "AND f.kind='file' AND f.state NOT IN ('done','cancelled')))";
        const after = this.retryAfter.get(room);
        let rows = after ? this.store.db.prepare(eligible +
            ' AND (o.at>? OR (o.at=? AND o.id>?)) ORDER BY o.at,o.id LIMIT 10')
            .all(room, after.at, after.at, after.id) : [];
        if (!rows.length)
            rows = this.store.db.prepare(eligible + ' ORDER BY o.at,o.id LIMIT 10').all(room);
        const last = rows.at(-1);
        if (last) this.retryAfter.set(room, { at: Number(last.at), id: String(last.id) });
        else this.retryAfter.delete(room);
        return rows.map(r => String(r.id));
    }
    pending(room: string): { id: string; state: string; stage: string; detail?: string }[] {
        return this.store.db.prepare("SELECT id,state,file_state,delivery_error FROM outbox WHERE room=? AND state NOT IN ('done','cancelled') ORDER BY at DESC LIMIT 20").all(room)
            .map(r => ({ id: String(r.id), state: String(r.state), stage: r.file_state !== 'none' && r.file_state !== 'done' ? 'file' : 'reply', ...(r.delivery_error ? { detail: String(r.delivery_error) } : {}) }));
    }
}
