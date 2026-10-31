import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile, readdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { Store } from '../src/store.ts';
import { Outbox } from '../src/outbox.ts';
import { Render } from '../src/render.ts';
import { Receipts } from '../src/receipts.ts';
import type { Transport, Room, ReceiptState, Message, Delivery } from '../src/types.ts';

const answer = 'A full answer.\n'.repeat(2500);
async function fixture() {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-full-reply-'));
    const path = join(dir, 'state.sqlite');
    const store = new Store(path);
    const room: Room = { key: 'room', chatId: 'chat', projectId: 'p', maintainers: ['author'] };
    const session = store.session(room.key);
    const message: Message = { id: 'trigger', chatId: 'chat', chatType: 'group', senderId: 'author', text: 'request', time: Date.now(), mentioned: true, resources: [] };
    const delivery: Delivery = { uuid: 'input', roomKey: room.key, sessionId: session.id, message, background: [], attachments: [] };
    const files: string[] = [], reactions: ReceiptState[] = [], cards: object[] = [];
    let creates = 0;
    const transport: Transport = {
        send: async () => 'notice', card: async (_r, card, replyTo) => { assert.equal(replyTo, message.id); creates++; cards.push(card); return 'card'; },
        update: async (id, card) => { assert.equal(id, 'card'); cards.push(card); },
        download: async () => Buffer.alloc(0), fetch: async () => undefined, history: async () => ({ messages: [] }),
        receipt: async (_id, state = 'processing') => { reactions.push(state); },
        file: async (_r, path, name) => { assert.equal(name, 'reply.md'); files.push(await readFile(path, 'utf8')); return 'file'; }
    };
    const receipts = new Receipts(store, transport);
    const outbox = new Outbox(store, transport, receipts);
    receipts.accept(room, session.id, message);
    await receipts.drain();
    const render = () => new Render(room, session.id, transport, store, outbox, 250, delivery);
    return { dir, path, store, room, session, message, delivery, files, reactions, cards, transport, receipts, outbox, render,
        creates: () => creates,
        cleanup: async () => { await receipts.drain(); store.close(); await rm(dir, { recursive: true, force: true }); } };
}

test('full-answer upload failure is persisted, visible and never marked Done', async () => {
    const f = await fixture();
    try {
        f.transport.file = async () => { throw new Error('unknown upload outcome'); };
        await f.render().finish(answer, true, f.dir, [f.message]);
        await f.receipts.drain();
        const row = f.store.db.prepare('SELECT * FROM outbox').get()!;
        assert.equal(row.state, 'uncertain'); assert.equal(row.file_state, 'uncertain');
        assert.equal(row.text, answer); assert.equal(row.target, 'card');
        assert.equal(f.reactions.includes('done'), false);
        assert.match(JSON.stringify(f.cards.at(-1)), /未标记完成/);
        assert.equal(f.outbox.pending(f.room.key)[0]!.stage, 'file');
        assert.deepEqual(f.outbox.retryable(f.room.key), []);
    } finally { await f.cleanup(); }
});

test('explicit scoped retry regenerates the full file after process restart without a second card', async () => {
    const f = await fixture();
    let second: Store | undefined;
    try {
        const original = f.transport.file;
        f.transport.file = async () => { throw new Error('ambiguous file send'); };
        await f.render().finish(answer, true, f.dir, [f.message]); await f.receipts.drain();
        const row = f.store.db.prepare('SELECT * FROM outbox').get()!;
        await rm(String(row.file_path), { force: true });
        // A distinct connection reconstructs all retry information from disk, not Render state.
        second = new Store(f.path); second.recover();
        const receipts = new Receipts(second, f.transport), outbox = new Outbox(second, f.transport, receipts);
        f.transport.file = original;
        assert.equal(await outbox.deliver(String(row.id), { ...f.room, key: 'other' }, true), false);
        assert.equal(await outbox.deliver(String(row.id), f.room), false);
        assert.equal(await outbox.deliver(String(row.id), f.room, true), true);
        await receipts.drain();
        assert.deepEqual(f.files, [answer]); assert.equal(f.creates(), 1);
        assert.equal(second.db.prepare('SELECT state,file_target FROM outbox').get()!.state, 'done');
        assert.equal(second.db.prepare('SELECT file_target FROM outbox').get()!.file_target, 'file');
        assert.ok(f.reactions.includes('done'));
        assert.equal(await outbox.deliver(String(row.id), f.room, true), false);
    } finally { second?.close(); await f.cleanup(); }
});

test('crash during file send is uncertain even when the card target is known', async () => {
    const f = await fixture();
    try {
        const id = f.outbox.save(f.room, f.session.id, answer, true, 'card', f.message.id, undefined, undefined, [f.message], { name: 'reply.md', directory: f.dir });
        f.store.db.prepare("UPDATE outbox SET state='sending',file_state='sending' WHERE id=?").run(id);
        f.store.recover();
        const row = f.store.db.prepare('SELECT state,file_state FROM outbox WHERE id=?').get(id)!;
        assert.equal(row.state, 'uncertain'); assert.equal(row.file_state, 'uncertain');
        assert.equal(await f.outbox.deliver(id, f.room), false);
        assert.equal(f.files.length, 0);
    } finally { await f.cleanup(); }
});

test('a recorded successful file is not sent again when finalization resumes', async () => {
    const f = await fixture();
    try {
        const id = f.outbox.save(f.room, f.session.id, answer, true, 'card', f.message.id, undefined, undefined, [f.message], { name: 'reply.md', directory: f.dir });
        f.store.db.prepare("UPDATE outbox SET state='sending',file_state='done',file_target='previous-file' WHERE id=?").run(id);
        f.store.recover();
        assert.equal(await f.outbox.deliver(id, f.room), true);
        assert.equal(f.files.length, 0);
        assert.equal(f.store.db.prepare('SELECT file_target FROM outbox').get()!.file_target, 'previous-file');
    } finally { await f.cleanup(); }
});

test('local export preparation failures are safe to retry and retain the original answer', async () => {
    const f = await fixture();
    try {
        const parent = join(f.dir, 'blocked'); await writeFile(parent, 'not-directory');
        const id = f.outbox.save(f.room, f.session.id, answer, true, 'card', f.message.id, undefined, undefined, [f.message], { name: 'reply.md', directory: parent });
        assert.equal(await f.outbox.deliver(id, f.room), false);
        assert.equal(f.store.db.prepare('SELECT state,file_state FROM outbox').get()!.state, 'pending');
        assert.equal(f.store.db.prepare('SELECT file_state FROM outbox').get()!.file_state, 'pending');
        assert.equal(f.files.length, 0); assert.deepEqual(f.outbox.retryable(f.room.key), [id]);
        await rm(parent);
        assert.equal(await f.outbox.deliver(id, f.room), true);
        assert.deepEqual(f.files, [answer]);
    } finally { await f.cleanup(); }
});

test('stop during full-file delivery suppresses completion without resetting the native session', async () => {
    const f = await fixture();
    try {
        let resolveUpload!: () => void, started!: () => void;
        const entered = new Promise<void>(r => { started = r; });
        f.transport.file = async () => { started(); await new Promise<void>(r => { resolveUpload = r; }); return 'late-file'; };
        const pending = f.render().finish(answer, true, f.dir, [f.message]);
        await entered;
        f.store.db.prepare("UPDATE outbox SET state='cancelled'").run();
        f.receipts.cancelSession(f.session.id);
        resolveUpload(); await pending; await f.receipts.drain();
        assert.equal(f.reactions.includes('done'), false);
        assert.equal(f.store.db.prepare('SELECT state FROM outbox').get()!.state, 'cancelled');
        assert.equal(f.store.session(f.room.key).id, f.session.id);
    } finally { await f.cleanup(); }
});

test('authorization is rechecked after card delivery before a required file is sent', async () => {
    const f = await fixture();
    try {
        let allowed = true;
        const outbox = new Outbox(f.store, f.transport, f.receipts, () => allowed);
        f.transport.update = async () => { allowed = false; };
        const id = outbox.save(f.room, f.session.id, answer, true, 'card', f.message.id, undefined, undefined, [f.message], { name: 'reply.md', directory: f.dir });
        assert.equal(await outbox.deliver(id, f.room), false); await f.receipts.drain();
        assert.equal(f.files.length, 0); assert.equal(f.reactions.includes('done'), false);
        assert.equal(f.store.db.prepare('SELECT state,text FROM outbox').get()!.state, 'cancelled');
        assert.equal(f.store.db.prepare('SELECT text FROM outbox').get()!.text, '');
    } finally { await f.cleanup(); }
});

test('successful short replies require no export while long ones wait for every part', async () => {
    const f = await fixture();
    try {
        await f.render().finish('short answer', true, f.dir, [f.message]); await f.receipts.drain();
        assert.equal(f.files.length, 0); assert.ok(f.reactions.includes('done'));
        assert.equal(f.store.db.prepare('SELECT file_state FROM outbox').get()!.file_state, 'none');
        assert.ok(!(await readdir(f.dir)).includes('exports'));
    } finally { await f.cleanup(); }
});

test('schema upgrade preserves prior completed deliveries without replaying them', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-outbox-migrate-'));
    const path = join(dir, 'old.sqlite');
    let store: Store | undefined;
    try {
        const old = new DatabaseSync(path);
        old.exec("CREATE TABLE outbox(id TEXT PRIMARY KEY,room TEXT NOT NULL,session TEXT NOT NULL,text TEXT NOT NULL,target TEXT,reply_to TEXT,thread_id TEXT,success INTEGER NOT NULL,state TEXT NOT NULL,at INTEGER NOT NULL); INSERT INTO outbox VALUES('old','room','session','answer','card',NULL,NULL,1,'done',1); PRAGMA user_version=2;");
        old.close();
        store = new Store(path); store.recover();
        const row = store.db.prepare('SELECT * FROM outbox').get()!;
        assert.equal(row.state, 'done'); assert.equal(row.file_state, 'none'); assert.equal(row.text, 'answer');
        assert.equal(store.db.prepare('PRAGMA user_version').get()!.user_version, 3);
    } finally { store?.close(); await rm(dir, { recursive: true, force: true }); }
});
