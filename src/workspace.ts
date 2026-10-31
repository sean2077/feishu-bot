import { mkdir, readdir, lstat, copyFile, chmod, writeFile, access, realpath } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { cleanEnv, runCommand } from './actions.ts';
import { hash, inside } from './util.ts';
import type { Config, Project, Room, Session } from './types.ts';
export interface Workspace {
    root: string;
    work: string;
    home: string;
    socketDir: string;
    trusted: string;
    inbound: string;
}
const excluded = new Set(['.git', 'node_modules', '.env', '.ssh', '.aws', '.azure', '.npmrc', '.pypirc', '.netrc', '.git-credentials', 'state', 'dist', '.claude']);
async function seedTree(source: string, target: string, limit: {
    files: number;
    bytes: number;
}): Promise<void> {
    for (const entry of await readdir(source, { withFileTypes: true })) {
        if (excluded.has(entry.name.toLowerCase()) || entry.name.toLowerCase().startsWith('.env.'))
            continue;
        const src = join(source, entry.name), dst = join(target, entry.name), st = await lstat(src);
        if (st.isSymbolicLink())
            continue; // Never follow symlinks into host private data.
        if (st.isDirectory()) {
            await mkdir(dst, { recursive: true });
            await seedTree(src, dst, limit);
        }
        else if (st.isFile()) {
            if (++limit.files > 20000 || (limit.bytes += st.size) > 512 * 1024 * 1024)
                throw new Error('workspace seed exceeds limit');
            await copyFile(src, dst);
            await chmod(dst, st.mode & 0o777);
        }
    }
}
export async function prepareWorkspace(config: Config, room: Room, session: Session, project: Project): Promise<Workspace> {
    // One physical writable checkout and native HOME per conversation boundary.
    const root = join(config.dataDir, 'rooms', hash(room.key).slice(0, 32));
    const work = join(root, 'workspace'), home = join(root, 'home'), socketDir = join(root, 'socket'), trusted = join(root, 'trusted'), inbound = join(root, 'inbound');
    for (const p of [root, work, home, socketDir, trusted, inbound])
        await mkdir(p, { recursive: true, mode: 0o700 });
    const marker = join(root, 'seeded');
    try {
        await access(marker);
    }
    catch {
        if (project.seedDir) {
            const source = await realpath(project.seedDir);
            if (inside(source, root) || inside(root, source))
                throw new Error('seedDir and state directory must not overlap');
            await seedTree(source, work, { files: 0, bytes: 0 });
        }
        if (project.trustedConfigDir)
            await seedTree(project.trustedConfigDir, trusted, { files: 0, bytes: 0 });
        if (config.runtime.mode === 'local' && project.trustedConfigDir) {
            const local = join(work, '.claude');
            await mkdir(local, { recursive: true, mode: 0o700 });
            await seedTree(project.trustedConfigDir, local, { files: 0, bytes: 0 });
        }
        if (project.seedDir) {
            const template = join(root, 'empty-git-template');
            await mkdir(template, { recursive: true, mode: 0o700 });
            const env = cleanEnv({ HOME: root, USERPROFILE: root, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: process.platform === 'win32' ? 'NUL' : '/dev/null', GIT_TEMPLATE_DIR: template });
            for (const args of [['init', '-b', 'main'], ['add', '-A'], ['-c', 'core.hooksPath=' + template, '-c', 'commit.gpgSign=false', '-c', 'user.name=Feishu Bot', '-c', 'user.email=feishu-bot@example.invalid', 'commit', '--allow-empty', '-m', 'Project source snapshot']]) {
                const result = await runCommand('git', args, { cwd: work, env, timeoutMs: 60000 });
                if (result.code !== 0)
                    throw new Error('Cannot initialize project source baseline');
            }
        }
        await writeFile(marker, '1\n', { mode: 0o600 });
    }
    // sessionId stays in native transcript state; room memory survives a deliberate /new.
    await writeFile(join(root, 'runtime-session'), session.id, { mode: 0o600 });
    return { root, work, home, socketDir, trusted, inbound };
}
export function workerPaths(config: Config, w: Workspace): {
    work: string;
    home: string;
    trusted: string;
} { return config.runtime.mode === 'docker' ? { work: '/workspace', home: '/home/worker', trusted: '/workspace/.claude' } : { work: w.work, home: w.home, trusted: w.trusted }; }
export function translateWorkerFile(config: Config, w: Workspace, path: string): string {
    if (config.runtime.mode === 'local')
        return resolve(w.work, path);
    const p = path.startsWith('/') ? path : `/workspace/${path}`;
    if (p !== '/workspace' && !p.startsWith('/workspace/'))
        throw new Error('file outside worker workspace');
    const relativePath = p.slice('/workspace/'.length);
    const result = resolve(w.work, relativePath);
    if (!inside(w.work, result))
        throw new Error('file outside worker workspace');
    return result;
}
