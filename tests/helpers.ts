import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { parseConfig } from '../src/config.ts';
import { Store } from '../src/store.ts';
import { Gateway } from '../src/gateway.ts';
import type { Args, Delivery, Message, Room, Transport, WorkerEvent, WorkerFactory } from '../src/types.ts';
export function rawConfig(dir: string): Args { return { tenantId: 'tenant', appId: 'cli_test', dataDir: dir, runtime: { mode: 'local' }, model: { baseUrl: 'https://gateway.example/anthropic', model: 'deepseek' }, projects: [{ id: 'p', maintainers: ['admin'], actions: [] }], groups: [{ chatId: 'group1', projectId: 'p' }, { chatId: 'group2', projectId: 'p' }], dms: [{ userId: 'alice', projectId: 'p' }, { userId: 'bob', projectId: 'p' }], limits: { updateMs: 250 } }; }
export function message(overrides: Partial<Message> = {}): Message { return { id: randomUUID(), chatId: 'group1', chatType: 'group', senderId: 'alice', text: 'test', time: Date.now(), mentioned: true, resources: [], ...overrides }; }
export class FakeTransport implements Transport {
    sent: {
        room: Room;
        text?: string;
        card?: object;
        id: string;
        replyTo?: string;
    }[] = [];
    updates: {
        id: string;
        card: object;
    }[] = [];
    receipts: string[] = [];
    quoted = new Map<string, Message>();
    downloads = new Map<string, Uint8Array>();
    async send(room: Room, text: string, replyTo?: string): Promise<string> { const id = randomUUID(); this.sent.push({ room, text, id, replyTo }); return id; }
    async card(room: Room, card: object, replyTo?: string): Promise<string> { const id = randomUUID(); this.sent.push({ room, card, id, replyTo }); return id; }
    async update(id: string, card: object): Promise<void> { this.updates.push({ id, card }); }
    async download(id: string): Promise<Uint8Array> { return this.downloads.get(id) ?? Buffer.from('attachment'); }
    async fetch(id: string): Promise<Message | undefined> { return this.quoted.get(id); }
    async receipt(id: string): Promise<void> { this.receipts.push(id); }
    async file(room: Room, path: string, name: string): Promise<string> { return this.send(room, `${path}:${name}`); }
}
export async function until(condition: () => boolean, timeout = 3000): Promise<void> { const start = Date.now(); while (!condition()) {
    if (Date.now() - start > timeout)
        throw new Error('condition did not become true');
    await new Promise(resolve => setTimeout(resolve, 5));
} }
export async function fixture() {
    const dir = await mkdtemp(join(tmpdir(), 'feishu-bot-'));
    const config = parseConfig(rawConfig(dir));
    const store = new Store(join(dir, 'state.sqlite'));
    const transport = new FakeTransport();
    const workers: {
        room: Room;
        sent: Delivery[];
        responses: {
            id: string;
            value: Args;
        }[];
        emit: (event: WorkerEvent) => void;
        stopped: boolean;
    }[] = [];
    const factory: WorkerFactory = async (room, session, emit) => {
        const w = { room, sent: [] as Delivery[], responses: [] as {
                id: string;
                value: Args;
            }[], emit, stopped: false };
        workers.push(w);
        return { send: async (delivery) => { w.sent.push(delivery); emit({ type: 'accepted', uuid: delivery.uuid }); }, respond: (id, value) => w.responses.push({ id, value }), stop: async () => { w.stopped = true; } };
    };
    const gateway = new Gateway(config, store, transport, factory);
    return { dir, config, store, transport, workers, gateway, close: async () => { await gateway.close(); store.close(); await rm(dir, { recursive: true, force: true }); } };
}
