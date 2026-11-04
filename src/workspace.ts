import { mkdir, readdir, lstat, copyFile, chmod, writeFile, access, realpath, stat } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { homedir } from 'node:os';
import { cleanEnv, runCommand } from './actions.ts';
import { hash, inside } from './util.ts';
import type { Config, Project, Room, Session, Workspace } from './types.ts';
export type { Workspace } from './types.ts';
const excluded = new Set(['.git', 'node_modules', '.env', '.ssh', '.aws', '.azure', '.npmrc', '.pypirc', '.netrc', '.git-credentials', 'state', 'dist', '.claude']);
async function seedTree(source: string, target: string, limit: { files: number; bytes: number }): Promise<void> {
    for (const entry of await readdir(source, { withFileTypes: true })) {
        if (excluded.has(entry.name.toLowerCase()) || entry.name.toLowerCase().startsWith('.env.')) continue;
        const src = join(source, entry.name), dst = join(target, entry.name), st = await lstat(src);
        if (st.isSymbolicLink()) continue;
        if (st.isDirectory()) {
            await mkdir(dst, { recursive: true });
            await seedTree(src, dst, limit);
        } else if (st.isFile()) {
            if (++limit.files > 20000 || (limit.bytes += st.size) > 512 * 1024 * 1024) throw new Error('workspace seed exceeds limit');
            await copyFile(src, dst); await chmod(dst, st.mode & 0o777);
        }
    }
}
async function inheritedWorkspace(w: Workspace): Promise<Workspace> {
    const home = await realpath(homedir());
    const configDir = await realpath(process.env.CLAUDE_CONFIG_DIR || join(home, '.claude'));
    if (!(await stat(configDir)).isDirectory()) throw new Error('Claude config directory must be an existing directory');
    return { ...w, home, nativeConfig: 'inherit', configDir, configDirOverride: !!process.env.CLAUDE_CONFIG_DIR?.trim() };
}
export async function prepareWorkspace(config: Config, room: Room, _session: Session, project: Project): Promise<Workspace> {
    const requestedRoot = join(config.dataDir, 'rooms', hash(room.key).slice(0, 32));
    await mkdir(requestedRoot, { recursive: true, mode: 0o700 });
    // Use the actual directory spelling for both native cwd and tool paths. Windows
    // temporary directories can contain 8.3 aliases which native permission checks
    // intentionally treat as suspicious. Resolve the alias; never bypass the check.
    const root = await realpath(requestedRoot);
    if (project.cwd) {
        const work = await realpath(project.cwd);
        if (!(await stat(work)).isDirectory()) throw new Error('project cwd must be an existing directory');
        if (inside(root, work) || inside(work, root)) throw new Error('project cwd and conversation state must not overlap');
        const inbound = join(root, 'inbound');
        await mkdir(inbound, { recursive: true, mode: 0o700 });
        if (project.nativeConfig === 'inherit') {
            return inheritedWorkspace({ root, work, home: '', inbound });
        }
        const home = join(root, 'home');
        await mkdir(home, { recursive: true, mode: 0o700 });
        return { root, work, home, inbound };
    }
    const work = join(root, 'workspace'), home = join(root, 'home'), inbound = join(root, 'inbound');
    for (const p of [root, work, home, inbound]) await mkdir(p, { recursive: true, mode: 0o700 });
    const marker = join(root, 'seeded');
    try { await access(marker); }
    catch {
        if (project.seedDir) {
            const source = await realpath(project.seedDir);
            if (inside(source, root) || inside(root, source)) throw new Error('seedDir and state directory must not overlap');
            await seedTree(source, work, { files: 0, bytes: 0 });
        }
        if (project.trustedConfigDir) {
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
                if (result.code !== 0) throw new Error('Cannot initialize project source baseline');
            }
        }
        await writeFile(marker, '1\n', { mode: 0o600 });
    }
    // This is data organization under a shared service account, not an OS sandbox.
    const workspace = { root, work, home, inbound };
    return project.nativeConfig === 'inherit' ? inheritedWorkspace(workspace) : workspace;
}
export function translateWorkerFile(_config: Config, w: Workspace, path: string): string {
    const result = resolve(w.work, path);
    if (!inside(w.work, result)) throw new Error('file outside worker workspace');
    return result;
}
/** File delivery can echo this chat's attachments without granting arbitrary exports. */
export async function translateWorkerOutput(w: Workspace, path: string): Promise<{ root: string; path: string }> {
    const result = resolve(w.work, path);
    const root = [w.work, w.inbound].find(root => inside(root, result));
    if (!root) throw new Error('file outside worker workspace or current conversation attachments');
    if (root === w.inbound) {
        const expected = resolve(w.root, 'inbound'), actual = await realpath(root);
        // w.root is the canonical room identity captured when its workspace was
        // prepared. Resolving an attacker-replaced inbound root must not grant
        // that new directory ownership merely because its children are inside it.
        if (!inside(expected, actual) || !inside(actual, expected)) throw new Error('conversation attachment directory was redirected');
    }
    return { root, path: result };
}
