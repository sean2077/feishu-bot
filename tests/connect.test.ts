import test from 'node:test';
import assert from 'node:assert/strict';
import { connectBeforeStop } from '../src/connect.ts';

test('startup never opens a Feishu connection when shutdown was already requested', async () => {
    const stop = new AbortController();
    stop.abort();
    let opened = 0;
    const connected = await connectBeforeStop({
        connect: async () => { opened++; },
        disconnect: async () => {}
    }, stop.signal);
    assert.equal(connected, false);
    assert.equal(opened, 0);
});

test('a connection finishing after shutdown is disconnected again and never becomes ready', async () => {
    const stop = new AbortController();
    let resolveConnect: () => void = () => {};
    const connect = new Promise<void>(resolve => { resolveConnect = resolve; });
    let disconnected = 0;
    const channel = {
        connect: () => connect,
        disconnect: async () => { disconnected++; }
    };
    const pending = connectBeforeStop(channel, stop.signal);
    stop.abort();
    assert.equal(await pending, false, 'SIGTERM should not wait indefinitely for a connection handshake');
    assert.equal(disconnected, 0, 'the caller owns the initial disconnect during shutdown');
    resolveConnect();
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(disconnected, 1, 'a late connection must not resurrect a stopped service');
});

test('successful connect reports ready; subsequent shutdown is handled by the caller', async () => {
    const stop = new AbortController();
    let disconnects = 0;
    const connected = await connectBeforeStop({
        connect: async () => {},
        disconnect: async () => { disconnects++; }
    }, stop.signal);
    assert.equal(connected, true);
    stop.abort();
    await Promise.resolve();
    assert.equal(disconnects, 0);
});

test('a genuine connection failure propagates for startup diagnostics', async () => {
    const stop = new AbortController();
    await assert.rejects(connectBeforeStop({
        connect: async () => { throw new Error('fixture connect rejected'); },
        disconnect: async () => {}
    }, stop.signal), /fixture connect rejected/);
});
