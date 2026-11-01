import test from 'node:test';
import assert from 'node:assert/strict';
import { Receipts } from '../src/receipts.ts';
import { reactionHandler } from '../src/feishu.ts';
import { route } from '../src/policy.ts';
import { fixture, message } from './helpers.ts';

// The remote API commits a reaction, but the response is lost. A retry must
// inspect this application's reactions before issuing another external write.
function ambiguousChannel(failEmoji: 'OnIt' | 'Done') {
    const own = new Map<string, string>();
    const foreign = new Map<string, string>([['foreign-onit', 'OnIt'], ['foreign-done', 'Done']]);
    const adds: string[] = [], lookups: string[] = [], removals: string[] = [];
    let loseResponse = true;
    const channel = {
        botIdentity: { openId: 'ou_fixture_bot' },
        addReaction: async (_id: string, emoji: string) => {
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
    const api = ambiguousChannel('OnIt');
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
    const api = ambiguousChannel('Done');
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
