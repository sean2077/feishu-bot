import { readFile } from 'node:fs/promises';
import { resolve, isAbsolute } from 'node:path';
import type { ActionSpec, Config, Project } from './types.ts';
function obj(v: unknown, at: string): Record<string, unknown> {
    if (!v || typeof v !== 'object' || Array.isArray(v))
        throw new Error(`${at}: expected object`);
    return v as Record<string, unknown>;
}
function keys(o: Record<string, unknown>, allowed: string[], at: string): void {
    for (const k of Object.keys(o))
        if (!allowed.includes(k))
            throw new Error(`${at}: unknown field ${k}`);
}
function text(v: unknown, at: string, fallback?: string): string {
    if (v === undefined && fallback !== undefined)
        return fallback;
    if (typeof v !== 'string' || !v.trim() || v.includes('\0'))
        throw new Error(`${at}: expected nonempty string`);
    return v;
}
function integer(v: unknown, at: string, fallback: number, min = 0, max = 1000000): number {
    if (v === undefined)
        return fallback;
    if (!Number.isSafeInteger(v) || (v as number) < min || (v as number) > max)
        throw new Error(`${at}: invalid integer`);
    return v as number;
}
function boolean(v: unknown, at: string, fallback: boolean): boolean {
    if (v === undefined)
        return fallback;
    if (typeof v !== 'boolean')
        throw new Error(`${at}: expected boolean`);
    return v;
}
function list(v: unknown, at: string): unknown[] {
    if (!Array.isArray(v))
        throw new Error(`${at}: expected array`);
    return v;
}
function strings(v: unknown, at: string): string[] { return list(v, at).map((s) => text(s, at)); }
function identifier(v: unknown, at: string): string {
    const s = text(v, at);
    if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$/.test(s))
        throw new Error(`${at}: invalid id`);
    return s;
}
function unique(values: string[], at: string): void {
    if (new Set(values).size !== values.length)
        throw new Error(`${at}: duplicate id`);
}
function stringMap(v: unknown, at: string): Record<string, string> {
    return Object.fromEntries(Object.entries(obj(v ?? {}, at)).map(([k, value]) => [k, text(value, `${at}.${k}`)]));
}
function action(value: unknown): ActionSpec {
    const a = obj(value, 'action');
    keys(a, ['id', 'description', 'executable', 'args', 'cwd', 'env', 'vars', 'readOnly', 'timeoutMs', 'parameters'], 'action');
    const parameters: ActionSpec['parameters'] = {};
    for (const [name, raw] of Object.entries(obj(a.parameters ?? {}, 'parameters'))) {
        identifier(name, 'parameter');
        const p = obj(raw, 'parameter');
        keys(p, ['description', 'pattern', 'enum', 'maxLength', 'workspaceFile'], 'parameter');
        const pattern = p.pattern === undefined ? undefined : text(p.pattern, 'pattern');
        if (pattern !== undefined) {
            if (pattern.length > 256)
                throw new Error('pattern too long');
            new RegExp(pattern, 'u');
        }
        parameters[name] = { description: p.description === undefined ? undefined : text(p.description, 'description'), pattern,
            workspaceFile: boolean(p.workspaceFile, 'workspaceFile', false), enum: p.enum === undefined ? undefined : strings(p.enum, 'enum'), maxLength: integer(p.maxLength, 'maxLength', 2048, 1, 32768) };
    }
    const executable = text(a.executable, 'executable');
    if (!isAbsolute(executable))
        throw new Error('action executable must be an absolute trusted path');
    const args = strings(a.args ?? [], 'args');
    for (const arg of args)
        for (const match of arg.matchAll(/\{\{([^}]+)\}\}/g)) {
            if (!(match[1]! in parameters))
                throw new Error(`undefined action parameter ${match[1]}`);
        }
    return { id: identifier(a.id, 'action.id'), description: text(a.description, 'description'), executable, args,
        cwd: a.cwd === undefined ? undefined : resolve(text(a.cwd, 'cwd')), env: stringMap(a.env, 'action.env'), vars: stringMap(a.vars, 'action.vars'),
        readOnly: boolean(a.readOnly, 'readOnly', false), timeoutMs: integer(a.timeoutMs, 'timeoutMs', 60000, 100, 600000), parameters };
}
export function parseConfig(value: unknown, baseDir = process.cwd()): Config {
    const c = obj(value, 'config');
    keys(c, ['tenantId', 'appId', 'appSecretEnv', 'dataDir', 'runtime', 'model', 'projects', 'groups', 'dms', 'limits'], 'config');
    const r = obj(c.runtime ?? {}, 'runtime');
    keys(r, ['mode', 'image', 'maxWorkers', 'idleMs', 'memory', 'cpus'], 'runtime');
    const mode = text(r.mode, 'runtime.mode', 'docker');
    if (mode !== 'local' && mode !== 'docker')
        throw new Error('runtime.mode must be local or docker');
    const m = obj(c.model, 'model');
    keys(m, ['baseUrl', 'tokenEnv', 'model', 'smallModel', 'extraHeaders', 'allowHttp', 'maxConcurrent'], 'model');
    const baseUrl = new URL(text(m.baseUrl, 'model.baseUrl'));
    const allowHttp = boolean(m.allowHttp, 'allowHttp', false);
    if (baseUrl.protocol !== 'https:' && !(allowHttp && baseUrl.protocol === 'http:'))
        throw new Error('model gateway must use HTTPS');
    if (baseUrl.username || baseUrl.password || baseUrl.hash || baseUrl.search)
        throw new Error('model.baseUrl must not contain credentials, query or fragment');
    const projects: Project[] = list(c.projects, 'projects').map(value => {
        const p = obj(value, 'project');
        keys(p, ['id', 'maintainers', 'seedDir', 'trustedConfigDir', 'autoApproveWorkspace', 'actions'], 'project');
        const maintainers = strings(p.maintainers, 'maintainers');
        if (!maintainers.length || maintainers.includes('*'))
            throw new Error('explicit maintainers required');
        const actions = list(p.actions ?? [], 'actions').map(action);
        unique(actions.map(a => a.id), 'actions');
        return { id: identifier(p.id, 'project.id'), maintainers,
            seedDir: p.seedDir === undefined ? undefined : resolve(baseDir, text(p.seedDir, 'seedDir')),
            trustedConfigDir: p.trustedConfigDir === undefined ? undefined : resolve(baseDir, text(p.trustedConfigDir, 'trustedConfigDir')),
            autoApproveWorkspace: boolean(p.autoApproveWorkspace, 'autoApproveWorkspace', mode === 'docker'), actions };
    });
    if (!projects.length)
        throw new Error('at least one project is required');
    unique(projects.map(p => p.id), 'projects');
    if (mode === 'local' && projects.some(p => p.autoApproveWorkspace))
        throw new Error('local mode cannot auto-approve shell/workspace tools; use Docker for a shared service');
    const groups = list(c.groups ?? [], 'groups').map(v => {
        const g = obj(v, 'group');
        keys(g, ['chatId', 'projectId', 'members'], 'group');
        return { chatId: identifier(g.chatId, 'chatId'), projectId: identifier(g.projectId, 'projectId'), members: g.members === undefined ? undefined : strings(g.members, 'members') };
    });
    const dms = list(c.dms ?? [], 'dms').map(v => {
        const d = obj(v, 'dm');
        keys(d, ['userId', 'projectId'], 'dm');
        return { userId: identifier(d.userId, 'userId'), projectId: identifier(d.projectId, 'projectId') };
    });
    unique(groups.map(g => g.chatId), 'groups');
    unique(dms.map(d => d.userId), 'dms');
    for (const b of [...groups, ...dms])
        if (!projects.some(p => p.id === b.projectId))
            throw new Error(`unknown project ${b.projectId}`);
    const l = obj(c.limits ?? {}, 'limits');
    keys(l, ['backgroundMessages', 'backgroundMs', 'textChars', 'inboxPerRoom', 'attachmentBytes', 'approvalMs', 'updateMs', 'messagesPerMinute'], 'limits');
    return {
        tenantId: identifier(c.tenantId, 'tenantId'), appId: identifier(c.appId, 'appId'),
        appSecretEnv: identifier(c.appSecretEnv ?? 'FEISHU_APP_SECRET', 'appSecretEnv'),
        dataDir: resolve(baseDir, text(c.dataDir, 'dataDir', 'state')),
        runtime: { mode, image: text(r.image, 'image', 'feishu-bot-worker:0.1.0'), maxWorkers: integer(r.maxWorkers, 'maxWorkers', 8, 1, 128),
            idleMs: integer(r.idleMs, 'idleMs', 0, 0, 86400000), memory: text(r.memory, 'memory', '2g'), cpus: integer(r.cpus, 'cpus', 2, 1, 64) },
        model: { baseUrl: baseUrl.toString().replace(/\/$/, ''), tokenEnv: identifier(m.tokenEnv ?? 'COMPANY_MODEL_TOKEN', 'tokenEnv'),
            model: text(m.model, 'model.model'), smallModel: text(m.smallModel, 'smallModel', text(m.model, 'model.model')),
            extraHeaders: stringMap(m.extraHeaders, 'extraHeaders'), allowHttp, maxConcurrent: integer(m.maxConcurrent, 'maxConcurrent', 16, 1, 128) },
        projects, groups, dms,
        limits: { backgroundMessages: integer(l.backgroundMessages, 'backgroundMessages', 30, 0, 200), backgroundMs: integer(l.backgroundMs, 'backgroundMs', 900000, 0, 86400000),
            textChars: integer(l.textChars, 'textChars', 20000, 100, 200000), inboxPerRoom: integer(l.inboxPerRoom, 'inboxPerRoom', 64, 1, 64),
            attachmentBytes: integer(l.attachmentBytes, 'attachmentBytes', 20 * 1024 * 1024, 1024, 100 * 1024 * 1024),
            approvalMs: integer(l.approvalMs, 'approvalMs', 300000, 1000, 3600000), updateMs: integer(l.updateMs, 'updateMs', 1000, 250, 30000),
            messagesPerMinute: integer(l.messagesPerMinute, 'messagesPerMinute', 30, 1, 1000) }
    };
}
export async function loadConfig(path: string): Promise<Config> {
    const { dirname } = await import('node:path');
    return parseConfig(JSON.parse(await readFile(path, 'utf8')), dirname(resolve(path)));
}
export function secretsFor(config: Config): string[] {
    const names = [config.appSecretEnv, config.model.tokenEnv, ...config.projects.flatMap(p => p.actions.flatMap(a => Object.values(a.env)))];
    return [...names.map(n => process.env[n]), ...Object.values(config.model.extraHeaders).map(v => v.startsWith('env:') ? process.env[v.slice(4)] : v)].filter((v): v is string => !!v);
}
