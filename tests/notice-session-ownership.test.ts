import test from 'node:test';
import assert from 'node:assert/strict';
import { fixture, message, until } from './helpers.ts';
import { route } from '../src/policy.ts';

test('a delayed informational reply keeps the originating session after /new selects another one', async () => {
    const f = await fixture();
    let release: ((id: string) => void) | undefined;
    try {
        const room = route(f.config, message())!;
        const original = f.store.session(room.key);
        const status = message({ text: '/status', senderId: 'alice' });
        const send = f.transport.send.bind(f.transport);
        f.transport.send = async (target, text, replyTo, threadId) =>
            replyTo === status.id ? new Promise<string>(resolve => { release = resolve; }) :
                send(target, text, replyTo, threadId);
        await f.gateway.message(status);
        await until(() => !!release);
        await f.gateway.message(message({ senderId: 'admin', text: '/new' }));
        const selected = f.store.session(room.key);
        assert.notEqual(selected.id, original.id, 'new must rotate the selected session');
        release!('om_delayed_status');
        await until(() => !!f.store.db.prepare('SELECT session FROM outputs WHERE id=?').get('om_delayed_status'));
        const row = f.store.db.prepare('SELECT room,session FROM outputs WHERE id=?').get('om_delayed_status')!;
        assert.equal(row.room, room.key);
        assert.equal(row.session, original.id, 'a late reply must not inherit the newly selected session');
        assert.equal(f.store.session(room.key).id, selected.id);
        assert.equal(f.workers.length, 0, 'status/new alone cannot call the model');
    } finally {
        release?.('om_delayed_status');
        await f.close();
    }
});

test('a status reply started without a session is not retrospectively attributed to a newly created session', async () => {
    const f = await fixture();
    let release: ((id: string) => void) | undefined;
    try {
        const room = route(f.config, message())!;
        assert.equal(f.store.listSessions(room.key).length, 0);
        const status = message({ text: '/status' });
        const send = f.transport.send.bind(f.transport);
        let delivered = false;
        f.transport.send = async (target, text, replyTo, threadId) => {
            if (replyTo !== status.id) return send(target, text, replyTo, threadId);
            return new Promise<string>(resolve => { release = id => { delivered = true; resolve(id); }; });
        };
        await f.gateway.message(status);
        await until(() => !!release);
        await f.gateway.message(message({ senderId: 'admin', text: '/new' }));
        assert.equal(f.store.listSessions(room.key).length, 1);
        release!('om_no_previous_session');
        await until(() => delivered);
        await new Promise(resolve => setTimeout(resolve, 50));
        assert.equal(f.store.db.prepare('SELECT id FROM outputs WHERE id=?').get('om_no_previous_session'), undefined,
            'read-only status with no originating session cannot be indexed into a later session');
        assert.equal(f.workers.length, 0);
    } finally {
        release?.('om_no_previous_session');
        await f.close();
    }
});
