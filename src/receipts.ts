import type { Message, Origin, ReceiptState, ReceiptApplied, Room, Transport } from './types.ts';
import { Store } from './store.ts';
import { Serial } from './util.ts';

/** Durable desired state plus per-message serialization closes late-reaction races. */
export class Receipts {
    private serial = new Serial();
    private retryAfter?: { at: number; id: string };
    readonly store: Store;
    readonly transport: Transport;
    readonly allowed?: (room: string, sender: string) => boolean;
    constructor(store: Store, transport: Transport, allowed?: (room: string, sender: string) => boolean) { this.store = store; this.transport = transport; this.allowed = allowed; }
    accept(room: Room, session: string, message: Message): void {
        this.store.db.prepare("INSERT INTO receipts(id,room,session,desired,at,sender) VALUES(?,?,?,'processing',?,?) ON CONFLICT(id) DO UPDATE SET desired='processing',at=excluded.at WHERE receipts.room=excluded.room AND receipts.session=excluded.session AND receipts.desired!='done'").run(message.id, room.key, session, Date.now(), message.senderId);
        this.kick(message.id);
    }
    settle(room: string, session: string, origins: Origin[], success: boolean): void {
        for (const { id } of origins) {
            this.store.db.prepare('UPDATE receipts SET desired=?,at=? WHERE id=? AND room=? AND session=?').run(success ? 'done' : 'clear', Date.now(), id, room, session);
            this.kick(id);
        }
    }
    cancelSession(session: string): void {
        this.store.db.prepare("UPDATE receipts SET desired='clear',at=? WHERE session=? AND desired='processing'").run(Date.now(), session);
        for (const row of this.store.db.prepare("SELECT id FROM receipts WHERE session=? AND desired='clear'").all(session)) this.kick(String(row.id));
    }
    recover(): void {
        // Preserve a pending external-add attempt: resetting applied to NULL would
        // lose the evidence needed to reconcile an OnIt accepted before restart.
        this.store.db.prepare("UPDATE receipts SET desired='clear' WHERE desired='processing'").run();
        this.retry();
    }
    retry(): void {
        // A permanently uncertain oldest batch must not starve newer reactions
        // after restart. Continue from the last scanned (at,id) key and wrap.
        const after = this.retryAfter;
        let rows = after ? this.store.db.prepare(
            'SELECT id,at FROM receipts WHERE (applied IS NULL OR desired!=applied) ' +
            'AND (at>? OR (at=? AND id>?)) ORDER BY at,id LIMIT 50'
        ).all(after.at, after.at, after.id) : [];
        if (!rows.length)
            rows = this.store.db.prepare(
                'SELECT id,at FROM receipts WHERE applied IS NULL OR desired!=applied ORDER BY at,id LIMIT 50'
            ).all();
        const last = rows.at(-1);
        this.retryAfter = last ? { at: Number(last.at), id: String(last.id) } : undefined;
        for (const row of rows) this.kick(String(row.id));
    }
    private kick(id: string): void {
        void this.serial.run(id, async () => {
            for (;;) {
                const row = this.store.db.prepare('SELECT desired,applied,room,sender FROM receipts WHERE id=?').get(id);
                if (!row || row.desired === row.applied) return;
                if (row.desired !== 'clear' && this.allowed && !this.allowed(String(row.room), String(row.sender ?? ''))) {
                    this.store.db.prepare("UPDATE receipts SET desired='clear' WHERE id=?").run(id);
                    continue;
                }
                const desired = row.desired as ReceiptState;
                const applied = row.applied as ReceiptApplied | undefined;
                // Finish removing OnIt before recording any attempt to add Done.
                // Otherwise a failed or stale OnIt lookup could leave a durable
                // attempt:done even though the Done API was never called.
                const next: ReceiptState = desired === 'done' &&
                    (applied === 'processing' || applied === 'attempt:processing') ? 'clear' : desired;
                // Persist the uncertain add BEFORE calling Feishu. A restart or
                // lost response must reconcile the owned reaction, not create it again.
                if (next === 'processing' || next === 'done') {
                    const attempt = `attempt:${next}`;
                    if (applied !== attempt)
                        this.store.db.prepare('UPDATE receipts SET applied=? WHERE id=?').run(attempt, id);
                }
                try { await this.transport.receipt(id, next, applied); }
                catch {
                    this.store.audit(String(row.room), 'receipt_retry_pending', { messageId: id });
                    return;
                }
                this.store.db.prepare('UPDATE receipts SET applied=? WHERE id=?').run(next, id);
                // Re-read: completion/cancellation may have arrived during an add request.
            }
        }).catch(() => {});
    }
    async drain(): Promise<void> { await this.serial.drain(); }
}
