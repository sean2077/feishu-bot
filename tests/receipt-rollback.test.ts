import test from 'node:test';
import assert from 'node:assert/strict';
import { Receipts } from '../src/receipts.ts';
import { reactionHandler } from '../src/feishu.ts';
import { route } from '../src/policy.ts';
import { fixture, message, until } from './helpers.ts';

test('a delayed own Done reaction is rolled back when the durable receipt becomes clear', async () => {
    const f = await fixture();
    const own = new Map<string, string>();
    const other = new Map<string, string>([['foreign-done', 'Done']]);
    const removed: string[] = [], lookups: string[] = [];
    let releaseDone!: () => void;
    let doneStarted = false;
    const inFlight = new Promise<void>(resolve => { releaseDone = resolve; });
    const channel = {
        botIdentity: { openId: 'ou_bot_fixture' },
        addReaction: async (_id: string, emoji: string) => {
            if (emoji === 'Done') {
                doneStarted = true;
                await inFlight;
                own.set('own-done', emoji);
                return 'own-done';
            }
            own.set('own-onit', emoji);
            return 'own-onit';
        },
        removeReaction: async (_id: string, rid: string) => { removed.push(rid); own.delete(rid); other.delete(rid); },
        rawClient: { im: { v1: { messageReaction: { list: async (req: { params: { reaction_type: string } }) => { lookups.push(req.params.reaction_type); return ({
            code: 0, data: { items: [
                ...[...own].filter(([, emoji]) => emoji === req.params.reaction_type)
                    .map(([reaction_id]) => ({ reaction_id, operator: { operator_type: 'app', operator_id: 'cli_fixture' } })),
                ...[...other].filter(([, emoji]) => emoji === req.params.reaction_type)
                    .map(([reaction_id]) => ({ reaction_id, operator: { operator_type: 'app', operator_id: 'other-app' } }))
            ], has_more: false }
        }); } } } } }
    };
    const receipts = new Receipts(f.store, { receipt: reactionHandler(channel as never, 'cli_fixture') } as never);
    try {
        const m = message(), room = route(f.config, m)!;
        const session = f.store.session(room.key);
        receipts.accept(room, session.id, m);
        await receipts.drain();
        assert.deepEqual([...own.values()], ['OnIt']);

        receipts.settle(room.key, session.id, [m], true);
        await until(() => doneStarted);
        // A later authorization/cancellation decision supersedes the Done RPC
        // while it is in flight. The Feishu adapter must remove its own Done,
        // not just the earlier processing reaction or a foreign app's emoji.
        receipts.settle(room.key, session.id, [m], false);
        releaseDone();
        await receipts.drain();
        assert.equal(f.store.db.prepare('SELECT desired FROM receipts WHERE id=?').get(m.id)?.desired, 'clear');
        assert.equal(f.store.db.prepare('SELECT applied FROM receipts WHERE id=?').get(m.id)?.applied, 'clear');
        assert.equal(own.size, 0, 'no stale Done must remain after a confirmed clear');
        assert.equal(other.get('foreign-done'), 'Done', 'other applications remain untouched');
        assert.ok(!removed.includes('foreign-done'), 'foreign reaction ids must never be removed');
        assert.deepEqual(lookups, ['Done'], 'do not add an OnIt scan in the confirmed-Done rollback path');
    } finally {
        releaseDone();
        await f.close();
    }
});
