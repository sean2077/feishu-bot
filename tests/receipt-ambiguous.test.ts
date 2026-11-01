import test from 'node:test';
import assert from 'node:assert/strict';
import { Receipts } from '../src/receipts.ts';
import { reactionHandler } from '../src/feishu.ts';
import { route } from '../src/policy.ts';
import { fixture, message } from './helpers.ts';

// The remote API commits a reaction, but the response is lost. A retry must
// inspect this application's reactions before issuing another external write.
function ambiguousChannel(failEmoji: 'OnIt' | 'Done', beforeAdd?: (emoji: string) => void) {
    const own = new Map<string, string>();
    const foreign = new Map<string, string>([['foreign-onit', 'OnIt'], ['foreign-done', 'Done']]);
    const adds: string[] = [], lookups: string[] = [], removals: string[] = [];
    let loseResponse = true;
    const channel = {
        botIdentity: { openId: 'ou_fixture_bot' },
        addReaction: async (_id: string, emoji: string) => {
            beforeAdd?.(emoji);
            adds.push(emoji);
            const reactionId = `own-${emoji}-${adds.length}`;
            own.set(reactionId, emoji);
            if (emoji === failEmoji && loseResponse) {
                loseResponse = false;
                throw new Error('fixture: accepted remotely, response lost');
            }
            return reactionId;
        },
        removeReaction: async (_id: string, reactionId: string) => {
            removals.push(reactionId);
            own.delete(reactionId);
            foreign.delete(reactionId);
        },
        rawClient: { im: { v1: { messageReaction: { list: async (request: { params: { reaction_type: string } }) => {
            const emoji = request.params.reaction_type;
            lookups.push(emoji);
            return { code: 0, data: { has_more: false, items: [
                ...[...own].filter(([, value]) => value === emoji).map(([reaction_id]) => ({
                    reaction_id, operator: { operator_type: 'app', operator_id: 'cli_fixture' }
                })),
                ...[...foreign].filter(([, value]) => value === emoji).map(([reaction_id]) => ({
                    reaction_id, operator: { operator_type: 'app', operator_id: 'other-app' }
                }))
            ] } };
        } } } } }
    };
    return { channel, own, foreign, adds, lookups, removals };
}

test('ambiguous OnIt add is reconciled rather than duplicated on retry', async () => {
    const f = await fixture();
    const api = ambiguousChannel('OnIt', () => {
        assert.equal(f.store.db.prepare("SELECT applied FROM receipts WHERE desired='processing'").get()?.applied,
            'attempt:processing', 'persist the attempt before the external call');
    });
    const receipts = new Receipts(f.store, { receipt: reactionHandler(api.channel as never, 'cli_fixture') } as never);
    try {
        const input = message(), room = route(f.config, input)!, session = f.store.session(room.key);
        receipts.accept(room, session.id, input);
        await receipts.drain();
        assert.equal(api.adds.filter(e => e === 'OnIt').length, 1);
        receipts.retry();
        await receipts.drain();
        assert.equal(api.adds.filter(e => e === 'OnIt').length, 1, 'remote OnIt must not be created twice');
        assert.equal([...api.own.values()].filter(e => e === 'OnIt').length, 1);
        assert.ok(api.lookups.includes('OnIt'), 'retry must reconcile the unknown result');
        assert.equal(f.store.db.prepare('SELECT applied FROM receipts WHERE id=?').get(input.id)?.applied, 'processing');
        assert.equal(api.foreign.size, 2, 'other apps are not modified');
    } finally { await f.close(); }
});

test('ambiguous Done add is reconciled across restart, and later clear removes only own reaction', async () => {
    const f = await fixture();
    const api = ambiguousChannel('Done', emoji => {
        const row = f.store.db.prepare('SELECT applied FROM receipts WHERE desired=?').get(emoji === 'Done' ? 'done' : 'processing');
        assert.equal(row?.applied, emoji === 'Done' ? 'attempt:done' : 'attempt:processing',
            'persist the attempt before the external call');
    });
    const transport = { receipt: reactionHandler(api.channel as never, 'cli_fixture') } as never;
    const receipts = new Receipts(f.store, transport);
    try {
        const input = message(), room = route(f.config, input)!, session = f.store.session(room.key);
        receipts.accept(room, session.id, input);
        await receipts.drain();
        receipts.settle(room.key, session.id, [input], true);
        await receipts.drain();
        assert.equal(api.adds.filter(e => e === 'Done').length, 1);
        // A replacement process no longer has the first adapter's in-memory IDs.
        const resumed = new Receipts(f.store, { receipt: reactionHandler(api.channel as never, 'cli_fixture') } as never);
        resumed.retry();
        await resumed.drain();
        assert.equal(api.adds.filter(e => e === 'Done').length, 1, 'remote Done must not be created twice');
        assert.equal([...api.own.values()].filter(e => e === 'Done').length, 1);
        assert.ok(api.lookups.includes('Done'), 'restart must check ownership before another create');
        assert.equal(f.store.db.prepare('SELECT applied FROM receipts WHERE id=?').get(input.id)?.applied, 'done');
        resumed.settle(room.key, session.id, [input], false);
        await resumed.drain();
        assert.equal([...api.own.values()].filter(e => e === 'Done').length, 0, 'a later clear revokes own Done');
        assert.ok(!api.removals.includes('foreign-done'));
        assert.equal(api.foreign.size, 2);
    } finally { await f.close(); }
});


// A successful remote write may not be visible in the immediately following
// reaction-list response. An empty read is not proof that the write failed.
for (const emoji of ['OnIt', 'Done'] as const) {
    test(`ambiguous ${emoji} remains pending until the owned reaction becomes visible`, async () => {
        const f = await fixture();
        const api = ambiguousChannel(emoji);
        const originalList = api.channel.rawClient.im.v1.messageReaction.list;
        let invisibleReads = 1;
        api.channel.rawClient.im.v1.messageReaction.list = async request => {
            const response = await originalList(request);
            if (request.params.reaction_type === emoji && invisibleReads-- > 0)
                return { ...response, data: { ...response.data, items: response.data.items.filter(item =>
                    item.operator.operator_id !== 'cli_fixture') } };
            return response;
        };
        const receipts = new Receipts(f.store, { receipt: reactionHandler(api.channel as never, 'cli_fixture') } as never);
        try {
            const input = message(), room = route(f.config, input)!, session = f.store.session(room.key);
            receipts.accept(room, session.id, input);
            await receipts.drain();
            if (emoji === 'Done') {
                receipts.settle(room.key, session.id, [input], true);
                await receipts.drain();
            }
            assert.equal(api.adds.filter(e => e === emoji).length, 1);
            receipts.retry();
            await receipts.drain();
            assert.equal(api.adds.filter(e => e === emoji).length, 1,
                'an empty listing immediately after an ambiguous write must not trigger another add');
            assert.equal(f.store.db.prepare('SELECT applied FROM receipts WHERE id=?').get(input.id)?.applied,
                emoji === 'Done' ? 'attempt:done' : 'attempt:processing',
                'keep the uncertain receipt retryable without pretending it was confirmed');
            receipts.retry();
            await receipts.drain();
            assert.equal(api.adds.filter(e => e === emoji).length, 1);
            assert.equal(f.store.db.prepare('SELECT applied FROM receipts WHERE id=?').get(input.id)?.applied,
                emoji === 'Done' ? 'done' : 'processing');
            assert.equal(api.foreign.size, 2, 'never modify foreign application reactions');
        } finally { await f.close(); }
    });
}

test('clearing an ambiguous Done waits for the remotely accepted reaction to become visible', async () => {
    const f = await fixture();
    const api = ambiguousChannel('Done');
    const transport = { receipt: reactionHandler(api.channel as never, 'cli_fixture') } as never;
    const receipts = new Receipts(f.store, transport);
    try {
        const input = message(), room = route(f.config, input)!, session = f.store.session(room.key);
        receipts.accept(room, session.id, input);
        await receipts.drain();
        receipts.settle(room.key, session.id, [input], true);
        await receipts.drain();
        assert.equal(f.store.db.prepare('SELECT applied FROM receipts WHERE id=?').get(input.id)?.applied,
            'attempt:done', 'Done was accepted remotely but its response was lost');
        assert.equal([...api.own.values()].filter(emoji => emoji === 'Done').length, 1);

        const originalList = api.channel.rawClient.im.v1.messageReaction.list;
        let invisibleReads = 1;
        api.channel.rawClient.im.v1.messageReaction.list = async request => {
            const response = await originalList(request);
            if (request.params.reaction_type === 'Done' && invisibleReads-- > 0)
                return { ...response, data: { ...response.data, items: response.data.items.filter(item =>
                    item.operator.operator_id !== 'cli_fixture') } };
            return response;
        };

        receipts.settle(room.key, session.id, [input], false);
        await receipts.drain();
        const pending = f.store.db.prepare('SELECT desired,applied FROM receipts WHERE id=?').get(input.id);
        assert.equal(pending?.desired, 'clear');
        assert.equal(pending?.applied, 'attempt:done',
            'one temporarily empty list cannot prove an ambiguous Done has been removed');
        assert.equal([...api.own.values()].filter(emoji => emoji === 'Done').length, 1);

        // Recreate both the handler and queue: reconciliation must also work after restart.
        const restarted = new Receipts(f.store, {
            receipt: reactionHandler(api.channel as never, 'cli_fixture')
        } as never);
        restarted.recover();
        await restarted.drain();
        assert.equal(f.store.db.prepare('SELECT applied FROM receipts WHERE id=?').get(input.id)?.applied, 'clear');
        assert.equal([...api.own.values()].filter(emoji => emoji === 'Done').length, 0);
        assert.equal(api.adds.filter(emoji => emoji === 'Done').length, 1, 'never repeat the unknown write');
        assert.equal(api.foreign.get('foreign-done'), 'Done', 'other app reactions must survive');
        assert.ok(!api.removals.includes('foreign-done'));
    } finally { await f.close(); }
});
