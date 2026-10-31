import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, readFile, realpath, symlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { prepareWorkspace } from '../src/workspace.ts';
import { route } from '../src/gateway.ts';
import { fixture, message } from './helpers.ts';

test('workspace paths resolve aliases without relocating data or changing conversation identity', async () => {
    const f = await fixture();
    try {
        const data = join(f.dir, 'canonical state');
        const alias = join(f.dir, 'STATE~1');
        await mkdir(data);
        await symlink(data, alias, process.platform === 'win32' ? 'junction' : 'dir');
        f.config.dataDir = alias;
        const room = route(f.config, message())!, session = f.store.session(room.key);
        const first = await prepareWorkspace(f.config, room, session, f.config.projects[0]!);
        assert.equal(first.root, await realpath(first.root));
        assert.equal(first.work, await realpath(first.work));
        assert.ok(!first.work.includes('STATE~1'));
        await writeFile(join(first.work, 'keep.txt'), 'existing conversation files');
        f.config.dataDir = data;
        const second = await prepareWorkspace(f.config, room, session, f.config.projects[0]!);
        assert.deepEqual(second, first);
        assert.equal(await readFile(join(second.work, 'keep.txt'), 'utf8'), 'existing conversation files');
        assert.equal(f.store.session(room.key).id, session.id);
    } finally { await f.close(); }
});
