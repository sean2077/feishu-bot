import { randomUUID } from 'node:crypto';
import { mkdir, writeFile, stat } from 'node:fs/promises';
import { join, basename } from 'node:path';
import type { Args, Config, Delivery, Message, Room, Session, Transport, Worker, WorkerEvent, WorkerFactory } from './types.ts';
import { Store } from './store.ts';
import { Outbox } from './outbox.ts';
import { Approvals, displayCard, type Question } from './approvals.ts';
import { ActionBroker } from './actions.ts';
import { hash, redact, safeFile, readBoundedFile, Serial, errorText } from './util.ts';
import { prepareWorkspace, translateWorkerFile, type Workspace } from './workspace.ts';
interface Slot {
    room: Room;
    session: Session;
    worker?: Worker;
    starting?: Promise<void>;
    abort: AbortController;
    live: boolean;
    busy: boolean;
    background: number;
    pinned: boolean;
    lastActivity: number;
    workspace?: Workspace;
    deliveries: Map<string, Delivery>;
    anchor?: Delivery;
    render?: Render;
    requests: Set<string>;
}
/** A coalescing display sink; API latency never blocks the native event reader. */
class Render {
    private text = '';
    private messageId?: string;
    private createUncertain = false;
    private timer?: ReturnType<typeof setTimeout>;
    private closed = false;
    private tail = Promise.resolve();
    private failures = 0;
    private last = '';
    readonly room: Room;
    readonly session: string;
    readonly transport: Transport;
    readonly store: Store;
    readonly replyTo?: string;
    readonly threadId?: string;
    readonly interval: number;
    constructor(room: Room, session: string, transport: Transport, store: Store, interval: number, anchor?: Delivery) { this.room = room; this.session = session; this.transport = transport; this.store = store; this.interval = interval; this.replyTo = anchor?.message.id; this.threadId = anchor?.message.threadId; }
    update(text: string, partial: boolean): void {
        if (this.closed)
            return;
        if (partial)
            this.text = (this.text + text).slice(-60000);
        else if (!this.text.endsWith(text))
            this.text = text.slice(-60000);
        if (!this.timer) {
            this.timer = setTimeout(() => { this.timer = undefined; this.flush(false); }, this.interval);
            this.timer.unref();
        }
    }
    async cancel(title = '已停止'): Promise<void> { this.closed = true; if (this.timer)
        clearTimeout(this.timer); await this.tail; if (this.messageId)
        await this.transport.update(this.messageId, displayCard(title, '以下是中断前输出，不代表任务完成。\n\n' + this.text.slice(0, 24000))).catch(() => { }); }
    private flush(final: boolean, success = true): void {
        this.tail = this.tail.then(async () => {
            const text = this.text || (!success ? '本轮执行失败，请检查状态。' : '已完成。');
            if ((!this.messageId && this.createUncertain) || (!final && text === this.last))
                return;
            const content = text.length > 24000 ? text.slice(0, 24000) + '\n\n（长回复将另附完整文件。）' : text;
            const card = displayCard(final ? (success ? '回复' : '执行失败') : '处理中', content);
            try {
                if (this.messageId)
                    await this.transport.update(this.messageId, card);
                else {
                    this.messageId = await this.transport.card(this.room, card, this.replyTo, this.threadId);
                    this.store.recordOutput(this.messageId, this.room.key, this.session);
                }
                this.last = text;
            }
            catch {
                this.failures++;
                if (!this.messageId) this.createUncertain = true;
                if (final) {
                    const id = await this.transport.send(this.room, content, this.replyTo, this.threadId);
                    this.store.recordOutput(id, this.room.key, this.session);
                }
                else if (this.failures > 2)
                    this.last = text;
            }
        }).catch(() => { this.store.audit(this.room.key, 'output_delivery_failed', { session: this.session }); });
    }
    async finish(text: string, success: boolean, dataDir: string): Promise<void> {
        if (this.closed)
            return;
        this.closed = true;
        if (this.timer)
            clearTimeout(this.timer);
        if (text)
            this.text = text.slice(0, 500000);
        await this.tail;
        const outbox = new Outbox(this.store, this.transport);
        const output = outbox.save(this.room, this.session, this.text, success, this.messageId, this.replyTo, this.threadId);
        if (this.createUncertain && !this.messageId) this.store.db.prepare("UPDATE outbox SET state='uncertain' WHERE id=?").run(output);
        await outbox.deliver(output, this.room);
        if (this.text.length > 24000) {
            const dir = join(dataDir, 'exports', hash(this.room.key).slice(0, 32));
            await mkdir(dir, { recursive: true, mode: 0o700 });
            const path = join(dir, `${randomUUID()}.md`);
            await writeFile(path, this.text, { mode: 0o600 });
            await this.transport.file(this.room, path, 'reply.md').catch(() => { });
        }
    }
}
export function route(config: Config, message: Message): Room | undefined {
    if (message.senderIsBot || !message.senderId || !message.chatId || !message.id)
        return;
    const binding = message.chatType === 'group' ? config.groups.find(g => g.chatId === message.chatId) : config.dms.find(d => d.userId === message.senderId);
    if (!binding)
        return;
    const project = config.projects.find(p => p.id === binding.projectId)!;
    const members = 'members' in binding ? binding.members : undefined;
    if (members && !members.includes(message.senderId) && !project.maintainers.includes(message.senderId))
        return;
    return { key: JSON.stringify([config.tenantId, config.appId, project.id, message.chatType, message.chatId, message.chatType === 'p2p' ? message.senderId : null]),
        chatId: message.chatId, projectId: project.id, maintainers: project.maintainers, members, dmUser: message.chatType === 'p2p' ? message.senderId : undefined };
}
export class Gateway {
    readonly config: Config;
    readonly store: Store;
    readonly transport: Transport;
    readonly approvals: Approvals;
    readonly broker: ActionBroker;
    private factory: WorkerFactory;
    private slots = new Map<string, Slot>();
    private rooms = new Map<string, Room>();
    private inputSerial = new Serial();
    private dispatchSerial = new Serial();
    private rates = new Map<string, number[]>();
    private tasks = new Set<Promise<unknown>>();
    private track<T>(task: Promise<T>): Promise<T> { this.tasks.add(task); void task.finally(() => this.tasks.delete(task)).catch(() => { }); return task; }
    private secrets: string[];
    private closing = false;
    private housekeeping: ReturnType<typeof setInterval>;
    constructor(config: Config, store: Store, transport: Transport, factory: WorkerFactory, secrets: string[] = []) {
        this.config = config;
        this.store = store;
        this.transport = transport;
        this.factory = factory;
        this.secrets = secrets;
        this.approvals = new Approvals(store, transport, config.limits.approvalMs);
        this.broker = new ActionBroker(store, this.approvals, secrets, join(config.dataDir, 'action-inputs'));
        this.housekeeping = setInterval(() => { if (!this.closing)
            void this.track(this.sweep()).catch(() => { }); }, 30000);
        this.housekeeping.unref();
    }
    async recover(): Promise<void> {
        const recovered = this.store.recover();
        for (const roomKey of [...new Set([...recovered.rooms, ...this.store.pendingRooms(), ...this.store.db.prepare("SELECT DISTINCT room FROM outbox WHERE state!='done'").all().map(r => r.room as string)])]) {
            // Re-derive ownership from current policy, never trust a persisted room's old ACL.
            const row = this.store.db.prepare("SELECT message FROM inbox WHERE room=? AND message!='{}' ORDER BY at DESC LIMIT 1").get(roomKey);
            if (!row)
                continue;
            const room = route(this.config, JSON.parse(row.message as string));
            if (!room || room.key !== roomKey)
                continue;
            this.rooms.set(room.key, room);
            if (recovered.rooms.includes(roomKey))
                await this.notice(room, '上次运行中断：已交给 CC 的输入和结果未知的外部操作不会自动重放。会话历史可恢复，请核实后再继续。');
            this.kick(room);
        }
    }
    async message(original: Message): Promise<void> {
        if (this.closing)
            return;
        const room = route(this.config, original);
        if (!room)
            return;
        await this.inputSerial.run(room.key, async () => {
            if (this.store.seen(room.key, original.id))
                return;
            const message = { ...original, text: original.text.slice(0, this.config.limits.textChars), resources: original.resources.slice(0, 12) };
            this.rooms.set(room.key, room);
            const triggered = message.chatType === 'p2p' || message.mentioned || !!(message.replyTo && this.store.isOutput(room.key, message.replyTo));
            if (!triggered) {
                if (this.config.limits.backgroundMessages > 0) {
                    this.store.insert(room.key, message);
                    this.trimBackground(room.key);
                }
                return;
            }
            const rateKey = room.key + ':' + message.senderId, now = Date.now(), recent = (this.rates.get(rateKey) ?? []).filter(t => t > now - 60000);
            if (recent.length >= this.config.limits.messagesPerMinute) {
                this.store.insert(room.key, message, 'cancelled');
                await this.notice(room, '消息过于频繁，请稍后再试。', message);
                return;
            }
            recent.push(now);
            this.rates.set(rateKey, recent);
            if (/^\/(help|status|sessions|new|resume|stop|answer|outputs|retry-output)(?:\s|$)/.test(message.text)) {
                this.store.insert(room.key, message, 'done');
                await this.command(room, message);
                return;
            }
            if (this.store.countPending(room.key) >= this.config.limits.inboxPerRoom) {
                this.store.insert(room.key, message, 'cancelled');
                await this.notice(room, '本群待交付输入已达上限，请查看 /status 或联系维护者。', message);
                return;
            }
            const session = this.store.session(room.key), background = this.store.background(room.key, now, this.config.limits.backgroundMs, this.config.limits.backgroundMessages);
            let contextChars = 0;
            for (let i = background.length - 1; i >= 0; i--) {
                contextChars += background[i]!.text.length;
                if (contextChars > Math.min(this.config.limits.textChars * 2, 40000)) {
                    background.splice(0, i + 1);
                    break;
                }
            }
            const delivery: Delivery = { uuid: randomUUID(), roomKey: room.key, sessionId: session.id, message, background, attachments: [] };
            this.store.transaction(() => { this.store.insert(room.key, message, 'queued'); this.store.queue(delivery); this.store.consumeBackground(room.key, background.map(m => m.id)); });
            void this.transport.receipt(message.id).catch(() => { });
            this.kick(room);
        });
    }
    private trimBackground(room: string): void {
        this.store.db.prepare("DELETE FROM inbox WHERE room=? AND status='background' AND id NOT IN (SELECT id FROM inbox WHERE room=? AND status='background' ORDER BY at DESC,id DESC LIMIT ?)").run(room, room, this.config.limits.backgroundMessages);
    }
    private kick(room: Room): void { void this.dispatchSerial.run(room.key, () => this.dispatch(room)).catch(e => { this.store.audit(room.key, 'dispatch_failed', { error: redact(errorText(e), this.secrets) }); void this.notice(room, '输入已保存，但运行启动或交付失败；请查看服务诊断。'); }); }
    private async dispatch(room: Room): Promise<void> {
        if (this.closing)
            return;
        const pending = this.store.pending(room.key);
        if (!pending.length)
            return;
        let slot = this.slots.get(room.key);
        if (!slot) {
            if (this.slots.size >= this.config.runtime.maxWorkers) {
                await this.notice(room, '运行容量已满，输入已保存。其他会话释放容量后再交付，不会中止正在监控的任务。');
                return;
            }
            const session = this.store.session(room.key);
            slot = { room, session, abort: new AbortController(), live: true, busy: false, background: 0, pinned: false, lastActivity: Date.now(), deliveries: new Map(), requests: new Set() };
            this.slots.set(room.key, slot);
            const captured = slot;
            try {
                slot.workspace = await prepareWorkspace(this.config, room, session, this.config.projects.find(p => p.id === room.projectId)!);
                slot.worker = await this.factory(room, session, event => this.event(captured, event));
            }
            catch (e) {
                this.slots.delete(room.key);
                slot.live = false;
                throw e;
            }
            if (!slot.live || this.closing) {
                await slot.worker.stop();
                return;
            }
        }
        for (const delivery of pending) {
            if (!slot.live || this.closing)
                return;
            if (delivery.sessionId !== slot.session.id) {
                this.store.mark(delivery.uuid, 'cancelled');
                continue;
            }
            try {
                if (delivery.message.replyTo) {
                    const quoted = await this.transport.fetch(delivery.message.replyTo).catch(() => undefined);
                    if (quoted?.chatId === room.chatId) {
                        delivery.quote = { ...quoted, text: quoted.text.slice(0, this.config.limits.textChars), resources: quoted.resources.slice(0, 12) };
                        this.store.insert(room.key, delivery.quote, 'context');
                    }
                }
                const project = this.config.projects.find(p => p.id === room.projectId)!;
                const w = slot.workspace!;
                for (const [index, resource] of delivery.message.resources.entries()) {
                    const bytes = await this.transport.download(delivery.message.id, resource);
                    if (bytes.byteLength > this.config.limits.attachmentBytes)
                        throw new Error('附件超过允许大小');
                    const name = basename(resource.name).replace(/[^\p{L}\p{N}_.-]/gu, '_').slice(0, 100) || 'attachment';
                    const path = join(w.inbound, `${hash(delivery.message.id).slice(0, 16)}-${index}-${name}`);
                    await writeFile(path, bytes, { mode: 0o600 });
                    delivery.attachments.push({ path, name: resource.name });
                }
                if (!slot.live || slot.abort.signal.aborted)
                    return;
                slot.deliveries.set(delivery.uuid, delivery);
                slot.anchor ??= delivery;
                slot.busy = true;
                slot.lastActivity = Date.now();
                // Mark before writing: a crash after an ambiguous write must not replay an external action.
                this.store.mark(delivery.uuid, 'sent');
                await slot.worker!.send(delivery);
            }
            catch (e) {
                this.store.mark(delivery.uuid, 'uncertain');
                await this.notice(room, `本条交付未确认，不会自动重放：${redact(errorText(e), this.secrets)}`, delivery.message);
            }
        }
    }
    private event(slot: Slot, event: WorkerEvent): void {
        if (!slot.live)
            return;
        slot.lastActivity = Date.now();
        switch (event.type) {
            case 'ready':
                if (!/^[a-f0-9]{8}-(?:[a-f0-9]{4}-){3}[a-f0-9]{12}$/i.test(event.ccId))
                    break;
                this.store.setCc(slot.session.id, event.ccId);
                slot.session.ccId = event.ccId;
                break;
            case 'accepted': break; // Transport acknowledgement is not proof the model has read it.
            case 'consumed':
                if (slot.deliveries.has(event.uuid))
                    this.store.mark(event.uuid, 'consumed');
                break;
            case 'activity':
                if (event.background !== undefined)
                    slot.background = event.background;
                if (event.pinned !== undefined)
                    slot.pinned = event.pinned;
                if (event.state)
                    slot.busy = event.state !== 'idle';
                if (event.tool && /Monitor|CronCreate|ScheduleWakeup/.test(event.tool))
                    slot.pinned = true;
                break;
            case 'text':
                slot.render ??= new Render(slot.room, slot.session.id, this.transport, this.store, this.config.limits.updateMs, slot.anchor);
                slot.render.update(redact(event.text, this.secrets), event.partial);
                break;
            case 'result': {
                const finished = event.uuids ?? (event.uuid ? [event.uuid] : []);
                for (const id of finished)
                    if (slot.deliveries.has(id)) {
                        this.store.mark(id, 'done');
                        slot.deliveries.delete(id);
                    }
                // Retain unknown deliveries; a background notification must not settle another human turn.
                slot.busy = slot.deliveries.size > 0;
                const render = slot.render ?? new Render(slot.room, slot.session.id, this.transport, this.store, this.config.limits.updateMs, slot.anchor);
                slot.render = undefined;
                slot.anchor = slot.deliveries.values().next().value;
                void this.track(render.finish(redact(event.text, this.secrets), event.success, this.config.dataDir)).catch(() => { });
                this.store.audit(slot.room.key, 'turn_result', { session: slot.session.id, success: event.success, usage: event.usage ?? {} });
                break;
            }
            case 'request':
                if (slot.requests.size >= 10000) {
                    slot.worker?.respond(event.id, { approved: false, error: 'session request budget exceeded' });
                    void this.notice(slot.room, '本会话请求数达到安全上限，请在任务结束后新建会话。');
                    break;
                }
                if (!slot.requests.has(event.id)) {
                    slot.requests.add(event.id);
                    void this.track(this.request(slot, event)).then(value => { if (slot.live)
                        slot.worker?.respond(event.id, value); }).catch(e => { if (slot.live)
                        slot.worker?.respond(event.id, { approved: false, ok: false, error: redact(errorText(e), this.secrets) }); });
                }
                break;
            case 'error':
                void this.notice(slot.room, redact(event.message, this.secrets));
                break;
            case 'exit': {
                slot.live = false;
                slot.abort.abort();
                this.approvals.cancelSession(slot.session.id);
                if (slot.render)
                    void this.track(slot.render.cancel('执行中断'));
                this.store.db.prepare("UPDATE inbox SET status='uncertain' WHERE session=? AND status IN ('sent','consumed')").run(slot.session.id);
                if (this.slots.get(slot.room.key) === slot)
                    this.slots.delete(slot.room.key);
                if (!this.closing)
                    for (const room of this.rooms.values())
                        if (room.key !== slot.room.key)
                            this.kick(room);
                break;
            }
        }
    }
    private async request(slot: Slot, event: Extract<WorkerEvent, {
        type: 'request';
    }>): Promise<Args> {
        const project = this.config.projects.find(p => p.id === slot.room.projectId)!;
        if (event.kind === 'action') {
            const w = slot.workspace!;
            return this.broker.execute(slot.room, slot.session.id, project, event.name, event.input, slot.abort.signal, w.work, path => translateWorkerFile(this.config, w, path));
        }
        if (event.kind === 'attachment') {
            const id = String(event.input.messageId ?? ''), key = String(event.input.key ?? '');
            const stored = this.store.db.prepare('SELECT message FROM inbox WHERE room=? AND id=?').get(slot.room.key, id);
            if (!stored)
                throw new Error('attachment message is not in this conversation');
            const message = JSON.parse(stored.message as string) as Message;
            const resource = message.resources?.find(r => r.key === key);
            if (!resource)
                throw new Error('unknown attachment');
            const data = await this.transport.download(id, resource);
            if (data.byteLength > this.config.limits.attachmentBytes)
                throw new Error('attachment exceeds size limit');
            const w = slot.workspace!;
            const name = `${hash(id + key).slice(0, 24)}-${basename(resource.name).replace(/[^\p{L}\p{N}_.-]/gu, '_').slice(0, 100)}`;
            const path = join(w.inbound, name);
            await writeFile(path, data, { mode: 0o600 });
            return { ok: true, path: this.config.runtime.mode === 'docker' ? '/attachments/' + name : path };
        }
        if (event.kind === 'send_file') {
            const w = slot.workspace!;
            const translated = translateWorkerFile(this.config, w, String(event.input.path ?? ''));
            const path = await safeFile(w.work, translated);
            if ((await stat(path)).size > this.config.limits.attachmentBytes)
                throw new Error('file exceeds send limit');
            const bytes = await readBoundedFile(w.work, path, this.config.limits.attachmentBytes);
            const dir = join(this.config.dataDir, 'exports', hash(slot.room.key).slice(0, 32));
            await mkdir(dir, { recursive: true, mode: 0o700 });
            const snapshot = join(dir, randomUUID());
            await writeFile(snapshot, bytes, { mode: 0o600 });
            if (slot.abort.signal.aborted)
                throw new Error('operation cancelled');
            const id = await this.transport.file(slot.room, snapshot, basename(String(event.input.name ?? basename(path))));
            this.store.recordOutput(id, slot.room.key, slot.session.id);
            return { ok: true, messageId: id };
        }
        const questions = event.kind === 'question' ? event.input.questions as Question[] : undefined;
        return this.approvals.request(slot.room, slot.session.id, event.name, event.input, `工具：${event.name}\n参数：\`${redact(JSON.stringify(event.input), this.secrets).slice(0, 5000)}\``, slot.abort.signal, questions);
    }
    private async command(room: Room, message: Message): Promise<void> {
        const [command, ...parts] = message.text.trim().split(/\s+/), arg = parts.join(' ');
        const maintainer = room.maintainers.includes(message.senderId);
        if (command === '/help') {
            await this.notice(room, '直接 @ 或回复机器人即可续话。/status 状态；/sessions 本聊天历史；/new 新会话；/resume 会话ID 恢复；/stop 停止本聊天全部运行；/answer 请求ID 回答。新建、恢复与停止限维护者（私聊本人也可管理会话）。', message);
            return;
        }
        if (command === '/status') {
            const slot = this.slots.get(room.key);
            await this.notice(room, JSON.stringify({ project: room.projectId, session: slot?.session.id, state: slot ? (slot.busy ? 'working' : 'connected') : 'offline', background: slot?.background ?? 0, monitorPinned: slot?.pinned ?? false, pendingOrUnconfirmed: this.store.countPending(room.key), approvals: slot ? this.approvals.size(slot.session.id) : 0 }, null, 2), message);
            return;
        }
        if (command === '/outputs') {
            await this.notice(room, JSON.stringify(new Outbox(this.store, this.transport).pending(room.key), null, 2), message);
            return;
        }
        if (command === '/retry-output') {
            if (!maintainer && room.dmUser !== message.senderId) {
                await this.notice(room, '需要维护者批准补发。', message);
                return;
            }
            const ok = await new Outbox(this.store, this.transport).deliver(arg, room, true);
            await this.notice(room, ok ? '已补发。此前结果不明确时可能出现重复消息。' : '未补发：记录不存在、已发送或渠道仍不可用。', message);
            return;
        }
        if (command === '/sessions') {
            await this.notice(room, this.store.listSessions(room.key).map(s => `${s.id} · ${s.state}`).join('\n') || '尚无历史会话', message);
            return;
        }
        if (command === '/answer') {
            const id = parts.shift() ?? '';
            await this.notice(room, this.approvals.answer(room, message.senderId, id, parts.join(' ')), message);
            return;
        }
        if (!maintainer && room.dmUser !== message.senderId) {
            await this.notice(room, '此操作需要项目维护者。', message);
            return;
        }
        if (command === '/resume') {
            // Validate ownership before touching the current running session.
            if (!this.store.listSessions(room.key).some(s => s.id === arg)) {
                await this.notice(room, '只能恢复本聊天列出的会话。', message);
                return;
            }
            await this.stopRoom(room);
            this.store.resume(room.key, arg);
            await this.notice(room, '已选择历史会话。下一条消息会恢复上下文；旧后台进程不会自动重启。', message);
            return;
        }
        if (command === '/stop' || command === '/new') {
            await this.stopRoom(room);
            if (command === '/new') {
                this.store.newSession(room.key);
                await this.notice(room, '已新建本聊天会话。', message);
            }
            else
                await this.notice(room, '已停止本聊天运行、后台进程和待交付输入。历史保留；下条消息使用新会话，需要旧上下文时显式 /resume。', message);
        }
    }
    private async stopRoom(room: Room): Promise<void> {
        const slot = this.slots.get(room.key);
        if (slot) {
            slot.live = false;
            slot.abort.abort();
            this.approvals.cancelSession(slot.session.id);
            this.store.stopSession(slot.session.id);
            await slot.render?.cancel();
            await slot.worker?.stop();
            this.slots.delete(room.key);
        }
        else {
            const active = this.store.listSessions(room.key).find(s => s.state === 'open');
            if (active)
                this.store.stopSession(active.id);
        }
        if (!this.closing)
            for (const other of this.rooms.values())
                if (other.key !== room.key)
                    this.kick(other);
    }
    private async notice(room: Room, text: string, message?: Message): Promise<void> {
        try {
            const id = await this.transport.send(room, text, message?.id, message?.threadId);
            const session = this.store.listSessions(room.key).find(s => s.state === 'open');
            if (session)
                this.store.recordOutput(id, room.key, session.id);
        }
        catch {
            this.store.audit(room.key, 'notice_failed', {});
        }
    }
    async sweep(now = Date.now()): Promise<void> {
        if (this.closing)
            return;
        this.store.trim(now, this.config.limits.backgroundMs);
        const outbox = new Outbox(this.store, this.transport);
        for (const room of this.rooms.values())
            for (const id of outbox.retryable(room.key))
                await outbox.deliver(id, room);
        for (const [key, values] of this.rates) {
            const alive = values.filter(t => t > now - 60000);
            if (alive.length)
                this.rates.set(key, alive);
            else
                this.rates.delete(key);
        }
        if (!this.config.runtime.idleMs)
            return;
        for (const slot of this.slots.values()) {
            if (!slot.busy && !slot.pinned && slot.background === 0 && !this.store.countPending(slot.room.key) && !this.approvals.size(slot.session.id) && now - slot.lastActivity > this.config.runtime.idleMs) {
                slot.live = false;
                slot.abort.abort();
                await slot.worker?.stop();
                this.slots.delete(slot.room.key);
                // Keep native session ID and history. Reclaim process, not conversation.
            }
        }
        for (const room of this.rooms.values())
            this.kick(room);
    }
    async close(): Promise<void> {
        this.closing = true;
        clearInterval(this.housekeeping);
        this.approvals.close();
        await Promise.allSettled([...this.slots.values()].map(async (slot) => { slot.live = false; slot.abort.abort(); await slot.render?.cancel(); await slot.worker?.stop(); }));
        await this.inputSerial.drain();
        await this.dispatchSerial.drain();
        await Promise.allSettled([...this.tasks]);
        this.slots.clear();
    }
}
