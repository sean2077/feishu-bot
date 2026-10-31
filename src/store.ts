import { DatabaseSync } from 'node:sqlite';
import { randomUUID } from 'node:crypto';
import type { Delivery, Message, Session } from './types.ts';
export class Store {
    readonly db: DatabaseSync;
    constructor(path: string) {
        this.db = new DatabaseSync(path);
        this.db.exec('PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;');
        this.db.exec(`
      CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, room TEXT NOT NULL, cc TEXT, state TEXT NOT NULL, created INTEGER NOT NULL);
      CREATE UNIQUE INDEX IF NOT EXISTS one_active ON sessions(room) WHERE state='open';
      CREATE TABLE IF NOT EXISTS inbox(room TEXT NOT NULL, id TEXT NOT NULL, message TEXT NOT NULL, status TEXT NOT NULL, at INTEGER NOT NULL, uuid TEXT, session TEXT, payload TEXT, PRIMARY KEY(room,id));
      CREATE INDEX IF NOT EXISTS inbox_status ON inbox(room,status,at);
      CREATE TABLE IF NOT EXISTS outputs(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, at INTEGER NOT NULL);
      CREATE TABLE IF NOT EXISTS audit(seq INTEGER PRIMARY KEY, at INTEGER NOT NULL, room TEXT NOT NULL, event TEXT NOT NULL, detail TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, kind TEXT NOT NULL, digest TEXT NOT NULL, state TEXT NOT NULL, expires INTEGER NOT NULL, actor TEXT);
      CREATE TABLE IF NOT EXISTS outbox(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, text TEXT NOT NULL, target TEXT, reply_to TEXT, thread_id TEXT, success INTEGER NOT NULL, state TEXT NOT NULL, at INTEGER NOT NULL);
      CREATE TABLE IF NOT EXISTS action_runs(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, action TEXT NOT NULL, digest TEXT NOT NULL, state TEXT NOT NULL, at INTEGER NOT NULL);
      CREATE TABLE IF NOT EXISTS receipts(id TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, desired TEXT NOT NULL, applied TEXT, at INTEGER NOT NULL, sender TEXT);
      CREATE TABLE IF NOT EXISTS context_refs(room TEXT NOT NULL, id TEXT NOT NULL, message TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(room,id));
    `);
        this.addColumn('receipts', 'sender', 'TEXT');
        this.addColumn('outbox', 'card', 'TEXT');
        this.addColumn('outbox', 'origins', "TEXT NOT NULL DEFAULT '[]'");
        this.addColumn('outbox', 'file_name', 'TEXT');
        this.addColumn('outbox', 'file_path', 'TEXT');
        this.addColumn('outbox', 'file_target', 'TEXT');
        this.addColumn('outbox', 'file_state', "TEXT NOT NULL DEFAULT 'none'");
        this.addColumn('outbox', 'delivery_error', 'TEXT');
        this.db.exec('PRAGMA user_version=3');
    }
    private addColumn(table: string, column: string, declaration: string): void {
        if (!this.db.prepare(`PRAGMA table_info(${table})`).all().some(r => r.name === column))
            this.db.exec(`ALTER TABLE ${table} ADD COLUMN ${column} ${declaration}`);
    }
    transaction<T>(fn: () => T): T { this.db.exec('BEGIN IMMEDIATE'); try {
        const result = fn();
        this.db.exec('COMMIT');
        return result;
    }
    catch (e) {
        this.db.exec('ROLLBACK');
        throw e;
    } }
    session(room: string): Session {
        const row = this.db.prepare("SELECT * FROM sessions WHERE room=? AND state='open'").get(room);
        return row ? this.sessionRow(row) : this.newSession(room);
    }
    private sessionRow(r: Record<string, unknown>): Session { return { id: r.id as string, roomKey: r.room as string, ccId: r.cc as string | undefined, state: r.state as Session['state'], createdAt: r.created as number }; }
    newSession(room: string): Session { return this.transaction(() => { this.db.prepare("UPDATE sessions SET state='archived' WHERE room=? AND state='open'").run(room); const id = randomUUID(), now = Date.now(); this.db.prepare('INSERT INTO sessions VALUES(?,?,NULL,?,?)').run(id, room, 'open', now); return { id, roomKey: room, state: 'open', createdAt: now }; }); }
    listSessions(room: string): Session[] { return this.db.prepare('SELECT * FROM sessions WHERE room=? ORDER BY created DESC LIMIT 20').all(room).map(r => this.sessionRow(r)); }
    resume(room: string, id: string): Session {
        const r = this.db.prepare('SELECT * FROM sessions WHERE room=? AND id=?').get(room, id);
        if (!r)
            throw new Error('会话不属于当前聊天');
        return this.transaction(() => { this.db.prepare("UPDATE sessions SET state='archived' WHERE room=? AND state='open'").run(room); this.db.prepare("UPDATE sessions SET state='open' WHERE room=? AND id=?").run(room, id); return { ...this.sessionRow(r), state: 'open' }; });
    }
    setCc(id: string, cc: string): void { this.db.prepare('UPDATE sessions SET cc=? WHERE id=?').run(cc, id); }
    stopSession(id: string): void { this.db.prepare("UPDATE inbox SET status='cancelled',payload=NULL WHERE session=? AND status IN ('queued','sent','consumed')").run(id); }
    insert(room: string, message: Message, status = 'background'): boolean { return this.db.prepare('INSERT OR IGNORE INTO inbox(room,id,message,status,at) VALUES(?,?,?,?,?)').run(room, message.id, JSON.stringify(message), status, message.time).changes > 0; }
    seen(room: string, id: string): boolean { return !!this.db.prepare('SELECT 1 FROM inbox WHERE room=? AND id=?').get(room, id); }
    background(room: string, now: number, maxAge: number, count: number): Message[] { return this.db.prepare("SELECT message FROM inbox WHERE room=? AND status='background' AND at>=? ORDER BY at DESC,id DESC LIMIT ?").all(room, now - maxAge, count).reverse().map(r => JSON.parse(r.message as string) as Message); }
    consumeBackground(room: string, ids: string[]): void { const s = this.db.prepare("UPDATE inbox SET status='context' WHERE room=? AND id=? AND status='background'"); for (const id of ids)
        s.run(room, id); }
    queue(delivery: Delivery): void { this.db.prepare("UPDATE inbox SET status='queued',uuid=?,session=?,payload=? WHERE room=? AND id=?").run(delivery.uuid, delivery.sessionId, JSON.stringify(delivery), delivery.roomKey, delivery.message.id); }
    pending(room: string): Delivery[] { return this.db.prepare("SELECT payload FROM inbox WHERE room=? AND status='queued' ORDER BY rowid").all(room).map(r => JSON.parse(r.payload as string) as Delivery); }
    pendingRooms(): string[] { return this.db.prepare("SELECT DISTINCT room FROM inbox WHERE status='queued'").all().map(r => r.room as string); }
    mark(uuid: string, status: 'sent' | 'consumed' | 'done' | 'uncertain' | 'cancelled'): void { this.db.prepare('UPDATE inbox SET status=? WHERE uuid=?').run(status, uuid); }
    countPending(room: string): number { return (this.db.prepare("SELECT count(*) AS n FROM inbox WHERE room=? AND status IN ('queued','sent','consumed')").get(room)!.n as number); }
    finishConsumed(session: string): void { this.db.prepare("UPDATE inbox SET status='done',payload=NULL WHERE session=? AND status='consumed'").run(session); }
    recover(): {
        rooms: string[];
        requests: number;
    } { return this.transaction(() => { const rooms = this.db.prepare("SELECT DISTINCT room FROM inbox WHERE status IN ('sent','consumed')").all().map(r => r.room as string); this.db.exec("UPDATE inbox SET status='uncertain' WHERE status IN ('sent','consumed'); UPDATE action_runs SET state='uncertain' WHERE state='running'; UPDATE outbox SET state=CASE WHEN target IS NULL OR file_state='sending' THEN 'uncertain' ELSE 'pending' END, file_state=CASE WHEN file_state='sending' THEN 'uncertain' ELSE file_state END WHERE state IN ('sending','preparing');"); const requests = this.db.prepare("UPDATE requests SET state='expired' WHERE state='pending'").run().changes as number; return { rooms, requests }; }); }
    rememberContext(room: string, messages: Message[]): void {
        const insert = this.db.prepare('INSERT OR REPLACE INTO context_refs VALUES(?,?,?,?)');
        for (const m of messages) insert.run(room, m.id, JSON.stringify({ ...m, text: '' }), Date.now());
        this.db.prepare('DELETE FROM context_refs WHERE room=? AND id NOT IN (SELECT id FROM context_refs WHERE room=? ORDER BY at DESC,rowid DESC LIMIT 200)').run(room, room);
    }
    contextMessage(room: string, id: string): Message | undefined {
        const row = this.db.prepare('SELECT message FROM context_refs WHERE room=? AND id=? UNION ALL SELECT message FROM inbox WHERE room=? AND id=? LIMIT 1').get(room, id, room, id);
        if (!row) return undefined;
        try { const m = JSON.parse(String(row.message)) as Message; return m.id ? m : undefined; } catch { return undefined; }
    }
    hasContextThread(room: string, threadId: string): boolean {
        return !!this.db.prepare("SELECT 1 FROM context_refs WHERE room=? AND json_extract(message,'$.threadId')=? UNION ALL SELECT 1 FROM inbox WHERE room=? AND json_extract(message,'$.threadId')=? LIMIT 1").get(room, threadId, room, threadId);
    }
    discardInput(room: string, id: string): void {
        // Keep only the id tombstone, never rejected body or resource metadata.
        this.db.prepare("UPDATE inbox SET status='cancelled',message='{}',payload=NULL WHERE room=? AND id=?").run(room, id);
    }
    recordOutput(id: string, room: string, session: string): void { this.db.prepare('INSERT OR REPLACE INTO outputs VALUES(?,?,?,?)').run(id, room, session, Date.now()); }
    isOutput(room: string, id: string): boolean { return !!this.db.prepare('SELECT 1 FROM outputs WHERE room=? AND id=?').get(room, id); }
    audit(room: string, event: string, detail: Record<string, unknown>): void { this.db.prepare('INSERT INTO audit(at,room,event,detail) VALUES(?,?,?,?)').run(Date.now(), room, event, JSON.stringify(detail)); }
    trim(now: number, backgroundMs: number): void {
        this.db.prepare('DELETE FROM context_refs WHERE at<?').run(now - 86400000);
        this.db.prepare("DELETE FROM receipts WHERE desired=applied AND desired!='processing' AND at<?").run(now - 7 * 86400000);
        this.db.prepare("DELETE FROM inbox WHERE status IN ('background','context') AND at<?").run(now - backgroundMs);
        this.db.prepare("UPDATE inbox SET message='{}',payload=NULL WHERE status IN ('done','cancelled','uncertain') AND at<?").run(now - 86400000);
        // Retain tombstones for 30 days to suppress platform retries after restarts.
        this.db.prepare("DELETE FROM inbox WHERE status IN ('done','cancelled','uncertain') AND at<?").run(now - 30 * 86400000);
        this.db.prepare('DELETE FROM outputs WHERE at<?').run(now - 30 * 86400000);
        this.db.prepare("DELETE FROM outbox WHERE state='done' AND at<?").run(now - 7 * 86400000);
    }
    close(): void { this.db.close(); }
}
