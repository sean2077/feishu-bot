import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:net';
import { WSClient, EventDispatcher } from '@larksuiteoapi/node-sdk';
import { WebSocketServer } from 'ws';
import { feishuSdkLogger } from '../src/feishu-diagnostics.ts';
import { until } from './helpers.ts';

test('locked native SDK terminates a silent local WebSocket before reconnecting and cannot reconnect after close', { timeout: 8000 }, async () => {
    const server = new WebSocketServer({ host: '127.0.0.1', port: 0 });
    await new Promise<void>(resolve => server.once('listening', resolve));
    const port = (server.address() as { port: number }).port;
    let connections = 0, live = 0, maximum = 0, pings = 0, discovery = 0;
    const signals: string[] = [];
    server.on('connection', socket => {
        connections++; live++; maximum = Math.max(maximum, live);
        socket.on('close', () => { live--; });
        socket.on('message', () => { pings++; }); // Deliberately no application pong.
    });
    const client = new WSClient({ appId: 'cli_0000000000000001', appSecret: 'fixture-not-real', loggerLevel: 4,
        logger: feishuSdkLogger((_event, detail) => { const signal = (detail as { signal?: string }).signal; if (signal) signals.push(signal); }),
        wsConfig: { pingTimeout: 0.05 }, handshakeTimeoutMs: 100,
        httpInstance: { request: async () => { discovery++; return { code: 0, data: {
            URL: `ws://127.0.0.1:${port}/?device_id=fixture&service_id=1`,
            ClientConfig: { PingInterval: 0.2, ReconnectCount: 20, ReconnectInterval: 0.02, ReconnectNonce: 0 }
        } }; } } as any });
    try {
        client.start({ eventDispatcher: new EventDispatcher({}) });
        await until(() => connections >= 2, 4000);
        assert.ok(pings > 0);
        assert.ok(signals.includes('ws_pong_timeout'));
        assert.equal(maximum, 1, 'old local server socket must close before the replacement opens');
        client.close({ force: true });
        const atClose = connections, discoveryAtClose = discovery;
        await new Promise(resolve => setTimeout(resolve, 250));
        assert.equal(connections, atClose);
        assert.equal(discovery, discoveryAtClose);
        assert.equal(client.getConnectionStatus().state, 'idle');
    } finally {
        client.close({ force: true });
        for (const socket of server.clients) socket.terminate();
        await new Promise<void>(resolve => server.close(() => resolve()));
    }
});

test('locked native SDK aborts a local handshake that never upgrades', { timeout: 5000 }, async () => {
    const sockets = new Set<import('node:net').Socket>();
    const server = createServer(socket => { sockets.add(socket); socket.on('data', () => {}); socket.on('close', () => sockets.delete(socket)); });
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
    const port = (server.address() as { port: number }).port, signals: string[] = [];
    const client = new WSClient({ appId: 'cli_0000000000000001', appSecret: 'fixture-not-real', loggerLevel: 4,
        logger: feishuSdkLogger((_event, detail) => { const signal = (detail as { signal?: string }).signal; if (signal) signals.push(signal); }),
        wsConfig: { pingTimeout: 0.05 }, handshakeTimeoutMs: 50,
        httpInstance: { request: async () => ({ code: 0, data: {
            URL: `ws://127.0.0.1:${port}/?device_id=fixture&service_id=1`,
            ClientConfig: { PingInterval: 1, ReconnectCount: 0, ReconnectInterval: 0.02, ReconnectNonce: 0 }
        } }) } as any });
    try {
        client.start({ eventDispatcher: new EventDispatcher({}) });
        await until(() => signals.includes('ws_handshake_timeout'), 2000);
        client.close({ force: true });
        await until(() => sockets.size === 0, 1000);
    } finally {
        client.close({ force: true });
        for (const socket of sockets) socket.destroy();
        await new Promise<void>(resolve => server.close(() => resolve()));
    }
});
