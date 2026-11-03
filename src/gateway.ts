import { randomUUID } from 'node:crypto';
import { mkdir, writeFile, stat, unlink } from 'node:fs/promises';
import { join, basename } from 'node:path';
import type { Args, Config, Delivery, Message, Room, Session, Transport, Worker, WorkerEvent, WorkerFactory, Origin } from './types.ts';
import { Store } from './store.ts';
import { Outbox, type Outgoing } from './outbox.ts';
import { Render } from './render.ts';
import { Receipts } from './receipts.ts';
import { ChatHistory } from './history.ts';
import { route, roomFromKey, authorizedAuthor, readable, triggers } from './policy.ts';
export { route } from './policy.ts';
import { Approvals, displayCard, type Question } from './approvals.ts';
import { ActionBroker } from './actions.ts';
import { hash, redact, safeFile, readBoundedFile, Serial, errorText } from './util.ts';
import { prepareWorkspace, translateWorkerFile, type Workspace } from './workspace.ts';
import { managementReply } from './management.ts';
import { imageBatchEligible, stageImageBatch, sealImageWindows, imageBatchMembers } from './image-batches.ts';
import { recordUsage } from './usage.ts';
interface Slot {
    room: Room;
    session: Session;
    worker?: Worker;
    abort: AbortController;
    live: boolean;
    busy: boolean;
    background: number;
    pinned: boolean;
    lastActivity: number;
    workspace?: Workspace;
    deliveries: Map<string, Delivery>;
    /** Native input UUID -> original image input UUIDs. Never substitutes durable original rows. */
    inputGroups: Map<string, string[]>;
    /** Worker queue acknowledgement, not pipe-write completion or model consumption. */
    acceptedSupplements: Set<string>;
    anchor?: Delivery;
    renders: Map<string, Render>;
    replyAnchors: Map<string, Delivery>;
    compacting?: string;
    requests: Set<string>;
    activeRequests: number;
    stopUnconfirmed?: boolean;
}
/** Release the per-room dispatch lock even when an adapter is still awaiting a network response. */
async function waitOrAbort<T>(start: () => Promise<T>, signal: AbortSignal): Promise<T> {
    if (signal.aborted) throw new Error('operation cancelled');
    let onAbort!: () => void;
    const aborted = new Promise<never>((_, reject) => {
        onAbort = () => reject(new Error('operation cancelled'));
        signal.addEventListener('abort', onAbort, { once: true });
    });
    try { return await Promise.race([start(), aborted]); }
    finally { signal.removeEventListener('abort', onAbort); }
}
export class Gateway {
    readonly config: Config;
    readonly store: Store;
    readonly transport: Transport;
    readonly approvals: Approvals;
    readonly broker: ActionBroker;
    readonly receipts: Receipts;
    readonly outbox: Outbox;
    readonly history: ChatHistory;
    private factory: WorkerFactory;
    private slots = new Map<string, Slot>();
    private rooms = new Map<string, Room>();
    private inputSerial = new Serial();
    private dispatchSerial = new Serial();
    private poolSerial = new Serial();
    private waitingNotified = new Set<string>();
    private rates = new Map<string, number[]>();
    private imageTimers = new Map<string, { deadline: number; timer: ReturnType<typeof setTimeout> }>();
    private tasks = new Set<Promise<unknown>>();
    private track<T>(task: Promise<T>): Promise<T> { this.tasks.add(task); void task.finally(() => this.tasks.delete(task)).catch(() => { }); return task; }
    private secrets: string[];
    private closing = false;
    private readonly noticeAbort = new AbortController();
    private housekeeping: ReturnType<typeof setInterval>;
    constructor(config: Config, store: Store, transport: Transport, factory: WorkerFactory, secrets: string[] = []) {
        this.config = config;
        this.store = store;
        this.transport = transport;
        this.factory = factory;
        this.secrets = secrets;
        this.receipts = new Receipts(store, transport, (key, sender) => {
            const room = roomFromKey(this.config, key);
            return !!room && authorizedAuthor(room, sender);
        });
        this.outbox = new Outbox(store, transport, this.receipts, (row, room) => this.outputAllowed(row, room));
        this.history = new ChatHistory(config, store, transport);
        this.approvals = new Approvals(store, transport, config.limits.approvalMs, key => roomFromKey(config, key));
        this.broker = new ActionBroker(store, this.approvals, secrets, join(config.dataDir, 'action-inputs'));
        this.housekeeping = setInterval(() => { if (!this.closing)
            void this.track(this.sweep()).catch(() => { }); }, 30000);
        this.housekeeping.unref();
    }
    private outputAllowed(row: Outgoing, room: Room): boolean {
        const current = roomFromKey(this.config, room.key);
        if (!current) return false;
        if (row.origins.length) return row.origins.every(o => authorizedAuthor(current, o.senderId));
        if (!row.replyTo) return true; // Unattributed native background output stays in the bound room.
        const source = this.store.contextMessage(room.key, row.replyTo);
        return !!source && readable(current, source);
    }
    private sanitize(room: Room, delivery: Delivery): boolean {
        const current = route(this.config, delivery.message);
        if (!current || current.key !== room.key || !triggers(delivery.message) || delivery.message.inputIssue) return false;
        if (delivery.supplementTo && delivery.command) return false;
        if (delivery.command && (delivery.command.name !== 'compact' ||
            !/^\/compact(?:\s|$)/.test(delivery.message.text.trim()) ||
            !current.maintainers.includes(delivery.message.senderId) && current.dmUser !== delivery.message.senderId)) return false;
        delivery.background = this.config.limits.backgroundMessages > 0
            ? delivery.background.filter(m => readable(current, m)).slice(-this.config.limits.backgroundMessages) : [];
        delivery.quote = undefined;
        delivery.attachments = [];
        delete delivery.batchInputs;
        return true;
    }
    private foregroundTarget(room: Room): Delivery | undefined {
        const slot = this.slots.get(room.key);
        if (!slot?.live || !slot.worker || !slot.busy || slot.compacting || slot.stopUnconfirmed) return undefined;
        return [...slot.deliveries.values()].find(d => !d.command && !d.supplementTo);
    }
    private supplementAlive(room: Room, delivery: Delivery): boolean {
        if (!delivery.supplementTo) return true;
        const slot = this.slots.get(room.key), target = slot?.deliveries.get(delivery.supplementTo);
        return !!slot?.live && !!slot.worker && slot.busy && !slot.compacting && !slot.stopUnconfirmed &&
            slot.session.id === delivery.sessionId && !!target && !target.command && !target.supplementTo;
    }
    private cancelSupplement(room: Room, delivery: Delivery, reason: string, deliverNotice = true,
        expectedStatus: 'queued' | 'sent' = 'queued'): boolean {
        // A sent input is cancellable only after the worker explicitly proves it never entered the native queue.
        if (!this.store.db.prepare('SELECT 1 FROM inbox WHERE room=? AND session=? AND uuid=? AND status=?')
            .get(room.key, delivery.sessionId, delivery.uuid, expectedStatus)) return false;
        const current = route(this.config, delivery.message);
        let noticeId: string | undefined;
        this.store.transaction(() => {
            this.store.mark(delivery.uuid, 'cancelled');
            if (current?.key === room.key && triggers(delivery.message)) {
                const text = `补充已取消：${reason} 未提交给模型，也不会转成新任务。`;
                noticeId = this.outbox.save(current, delivery.sessionId, text, false, undefined, delivery.message.id,
                    delivery.message.threadId, displayCard('补充已取消', text), [delivery.message]);
                this.store.db.prepare("UPDATE outbox SET kind='recovery' WHERE id=?").run(noticeId);
            } else this.store.discardInput(room.key, delivery.message.id);
        });
        this.receipts.settle(room.key, delivery.sessionId, [delivery.message], false);
        if (noticeId && deliverNotice && !this.closing)
            void this.track(this.outbox.deliver(noticeId, current!)).catch(() => {});
        return true;
    }
    private cancelLostSupplements(room: Room): void {
        for (const delivery of this.store.pending(room.key))
            if (delivery.supplementTo && !this.supplementAlive(room, delivery))
                this.cancelSupplement(room, delivery, '目标前台运行已经结束或停止。');
    }
    private clearImageTimer(room: string): void {
        const pending = this.imageTimers.get(room);
        if (pending) clearTimeout(pending.timer);
        this.imageTimers.delete(room);
    }
    /** Timer is only a wakeup hint; the deadline and every original input live in SQLite. */
    private waitForImages(room: Room, delivery: Delivery): boolean {
        const batch = delivery.imageBatch;
        if (!batch || batch.closed || !this.config.limits.imageBatchMs || batch.deadline <= Date.now()) {
            this.clearImageTimer(room.key);
            return false;
        }
        if (this.imageTimers.get(room.key)?.deadline !== batch.deadline) {
            this.clearImageTimer(room.key);
            const timer = setTimeout(() => {
                this.imageTimers.delete(room.key);
                if (!this.closing) this.kick(room);
            }, Math.max(1, Math.min(5000, batch.deadline - Date.now())));
            timer.unref();
            this.imageTimers.set(room.key, { deadline: batch.deadline, timer });
        }
        return true;
    }
    private inputAuthorized(room: Room, delivery: Delivery): boolean {
        return route(this.config, delivery.message)?.key === room.key && triggers(delivery.message) && !delivery.message.inputIssue;
    }
    private queued(delivery: Delivery): boolean {
        return !!this.store.db.prepare("SELECT 1 FROM inbox WHERE room=? AND uuid=? AND session=? AND status='queued'")
            .get(delivery.roomKey, delivery.uuid, delivery.sessionId);
    }
    private rejectImageBatch(room: Room, members: Delivery[], explain: boolean): void {
        const remaining = members.filter(d => this.queued(d));
        if (!remaining.length) return;
        let noticeId: string | undefined;
        const first = remaining[0]!;
        this.store.transaction(() => {
            for (const delivery of remaining) {
                if (explain) this.store.mark(delivery.uuid, 'cancelled');
                else this.store.discardInput(room.key, delivery.message.id);
            }
            if (explain) {
                const text = '图片批次未提交：附件读取失败或超过图片／大小限制。整批未交给模型，请检查文件和配置后重新发送。';
                noticeId = this.outbox.save(room, first.sessionId, text, false, undefined, first.message.id,
                    first.message.threadId, displayCard('图片批次未提交', text), remaining.map(d => d.message));
                this.store.db.prepare("UPDATE outbox SET kind='recovery' WHERE id=?").run(noticeId);
            }
        });
        for (const delivery of remaining) this.receipts.settle(room.key, delivery.sessionId, [delivery.message], false);
        if (noticeId && !this.closing) void this.track(this.outbox.deliver(noticeId, room)).catch(() => {});
    }
    async recover(): Promise<void> {
        const recovered = this.store.recover(state => {
            // A crash must not commit expired requests without also recording
            // the warning needed to explain why those requests are unusable.
            for (const key of new Set([...state.rooms, ...state.requestRooms])) {
                const room = roomFromKey(this.config, key);
                if (!room) continue;
                const warnings: string[] = [];
                if (state.rooms.includes(key)) {
                    const hasAuthorizedUncertain = this.store.db.prepare("SELECT message FROM inbox WHERE room=? AND status='uncertain' AND message!='{}'").all(key)
                        .some(r => { try { return route(this.config, JSON.parse(String(r.message)))?.key === key; } catch { return false; } });
                    if (hasAuthorizedUncertain) warnings.push('上次运行中断。将继续原会话；未确认完成的输入与外部操作不会自动重放，请先核实结果。');
                }
                if (state.requestRooms.includes(key))
                    warnings.push('上次未完成的审批／追问已失效，旧卡片不能继续授权操作。若外部操作结果未知，请先核实，勿直接重做。');
                if (!warnings.length) continue;
                const text = warnings.join('\n\n');
                const sessions = this.store.listSessions(key);
                const session = sessions.find(item => item.state === 'open') ?? sessions[0];
                if (!session) this.store.audit(key, 'recovery_notice_unbound', {});
                this.outbox.saveRecovery(room, session?.id ?? '', text, displayCard('恢复提醒', text));
            }
        });
        if (this.config.limits.backgroundMessages === 0) this.store.db.exec("DELETE FROM inbox WHERE status='background'");
        const keys = new Set([...recovered.rooms, ...recovered.requestRooms, ...this.store.pendingRooms(),
            ...this.store.db.prepare("SELECT DISTINCT room FROM outbox WHERE state NOT IN ('done','cancelled')").all().map(r => String(r.room))]);
        for (const key of keys) {
            const room = roomFromKey(this.config, key);
            for (const delivery of this.store.pending(key)) {
                if (!room || !this.sanitize(room, delivery)) {
                    this.store.discardInput(key, delivery.message.id);
                    this.receipts.settle(key, delivery.sessionId, [delivery.message], false);
                } else if (delivery.supplementTo) {
                    this.cancelSupplement(room, delivery, '服务已重启，原前台运行不再存在。', false);
                } else this.store.queue(delivery);
            }
            if (!room) continue;
            this.rooms.set(key, room);
        }
        this.receipts.recover();
        for (const room of this.rooms.values()) {
            // Never-started persisted notices are safe to deliver; uncertain creates are excluded.
            const unsent = this.store.db.prepare(
                "SELECT id FROM outbox WHERE room=? AND kind='recovery' AND state='pending' AND target IS NULL ORDER BY at,id LIMIT 10"
            ).all(room.key);
            for (const row of unsent) await this.outbox.deliver(String(row.id), room);
            this.kick(room);
        }
    }
    async message(original: Message): Promise<void> {
        if (this.closing) return;
        const room = route(this.config, original);
        if (!room || (!triggers(original) && this.config.limits.backgroundMessages === 0)) return;
        await this.inputSerial.run(room.key, async () => {
            if (this.store.seen(room.key, original.id)) return;
            const message = { ...original, text: original.text.trim().slice(0, this.config.limits.textChars), resources: original.resources.slice(0, 12) };
            this.rooms.set(room.key, room);
            const triggered = triggers(message);
            if (!triggered) {
                if (this.config.limits.backgroundMessages > 0 && !message.inputIssue) {
                    this.store.insert(room.key, message);
                    this.trimBackground(room.key);
                }
                return;
            }
            // Read commands seal a window but do not dispatch work. The existing
            // durable deadline still wakes that batch without making a query start a worker.
            const boundary = !imageBatchEligible(message);
            if (boundary) this.store.transaction(() => sealImageWindows(this.store, room.key));
            let dispatchNeeded = false;
            try {
                // An authorized stop must remain available even after normal message traffic
                // reaches its limit. Other commands and non-maintainer input still consume it.
                const authorizedStop = /^\/stop(?:\s|$)/.test(message.text) &&
                    (room.maintainers.includes(message.senderId) || room.dmUser === message.senderId);
                const now = Date.now();
                if (!authorizedStop) {
                    const rateKey = room.key + ':' + message.senderId;
                    const recent = (this.rates.get(rateKey) ?? []).filter(t => t > now - 60000);
                    if (recent.length >= this.config.limits.messagesPerMinute) {
                        this.store.insert(room.key, message, 'cancelled');
                        this.notify(room, '消息过于频繁，请稍后再试。', message);
                        return;
                    }
                    recent.push(now);
                    this.rates.set(rateKey, recent);
                }
                if (message.inputIssue || original.text.trim().length > this.config.limits.textChars || original.resources.length > 12) {
                    this.store.insert(room.key, { ...message, text: '', resources: [] }, 'cancelled');
                    this.notify(room, `消息未提交：${message.inputIssue ?? '正文或附件数量超过本次输入上限，请分开发送。'}`, message);
                    return;
                }
                if (/^\/(help|status|sessions|new|resume|stop|answer|outputs|retry-output|usage)(?:\s|$)/.test(message.text)) {
                    this.store.insert(room.key, message, 'done');
                    // A read-only command has a synchronous snapshot, but its Feishu
                    // reply must not hold admission or block a later /stop.
                    if (/^\/(help|status|sessions|outputs|usage)(?:\s|$)/.test(message.text))
                        void this.track(this.command(room, message)).catch(error => {
                            try { this.store.audit(room.key, 'management_read_failed',
                                { error: redact(errorText(error), this.secrets) }); } catch { /* storage unavailable */ }
                        });
                    else await this.command(room, message);
                    return;
                }
                const supplement = /^\/(btw|ps)(?:\s+([\s\S]*))?$/.exec(message.text);
                let supplementTo: string | undefined;
                if (supplement) {
                    const body = supplement[2]?.trim();
                    if (!body) {
                        this.store.insert(room.key, message, 'done');
                        this.notify(room, '用法：/btw 补充正文（/ps 同义）。仅补充当前正在执行的前台任务。', message);
                        return;
                    }
                    const target = this.foregroundTarget(room);
                    if (!target) {
                        this.store.insert(room.key, message, 'cancelled');
                        this.notify(room, '当前没有正在执行的前台任务，补充未提交。需要开始任务时请发送普通消息。', message);
                        return;
                    }
                    supplementTo = target.uuid;
                    message.text = body; // Never parse the body again as a management command.
                }
                if (this.store.countPending(room.key) >= this.config.limits.inboxPerRoom) {
                    this.store.insert(room.key, message, 'cancelled');
                    this.notify(room, '本群待交付输入已达上限，请查看 /status 或联系维护者。', message);
                    return;
                }
                const compactMatch = supplement ? null : /^\/compact(?:\s+([\s\S]*))?$/.exec(message.text);
                if (compactMatch && !room.maintainers.includes(message.senderId) && room.dmUser !== message.senderId) {
                    this.store.insert(room.key, message, 'cancelled');
                    this.notify(room, '整理共享会话需要维护者操作。', message);
                    return;
                }
                const session = this.store.session(room.key), background = compactMatch || supplement ? [] : this.store.background(room.key, now, this.config.limits.backgroundMs, this.config.limits.backgroundMessages)
                    .filter(m => readable(room, m));
                let contextChars = 0;
                for (let i = background.length - 1; i >= 0; i--) {
                    contextChars += background[i]!.text.length;
                    if (contextChars > Math.min(this.config.limits.textChars * 2, 40000)) { background.splice(0, i + 1); break; }
                }
                const delivery: Delivery = { uuid: randomUUID(), roomKey: room.key, sessionId: session.id, message, background, attachments: [], supplementTo,
                    command: compactMatch ? { name: 'compact', instructions: (compactMatch[1] ?? '').slice(0, 4000) } : undefined };
                this.store.transaction(() => {
                    stageImageBatch(this.store, delivery, this.config.limits, now);
                    this.store.insert(room.key, message, 'queued');
                    this.store.queue(delivery);
                    this.store.consumeBackground(room.key, background.map(m => m.id));
                });
                dispatchNeeded = true;
                this.receipts.accept(room, session.id, message);
                if (compactMatch && (this.slots.get(room.key)?.busy || this.store.countPending(room.key) > 1))
                    this.notify(room, '已接收 /compact，将在当前工作轮次结束后整理上下文；期间仍可处理审批。', message);
                if (supplementTo && this.store.pending(room.key).some(d => d.command))
                    this.notify(room, '补充已保存，等待前面的 /compact 控制边界；若目标运行先结束则取消，不会另起任务。审批仍可正常操作。', message);
            } finally {
                if (dispatchNeeded && !this.closing) this.kick(room);
            }
        });
    }
    private trimBackground(room: string): void {
        this.store.db.prepare("DELETE FROM inbox WHERE room=? AND status='background' AND id NOT IN (SELECT id FROM inbox WHERE room=? AND status='background' ORDER BY at DESC,id DESC LIMIT ?)").run(room, room, this.config.limits.backgroundMessages);
    }
    private kick(room: Room): void {
        void this.dispatchSerial.run(room.key, () => this.dispatch(room)).catch(e => {
            this.store.audit(room.key, 'dispatch_failed', { error: redact(errorText(e), this.secrets) });
            if (!this.closing) void this.track(this.notice(room, '输入已保存，但运行启动或交付失败；请查看服务诊断。'));
        });
    }
    private async dispatch(room: Room): Promise<void> {
        if (this.closing) return;
        this.cancelLostSupplements(room);
        const pending = this.store.pending(room.key).filter(delivery => {
            if (this.sanitize(room, delivery)) { this.store.queue(delivery); return true; }
            this.store.discardInput(room.key, delivery.message.id);
            this.receipts.settle(room.key, delivery.sessionId, [delivery.message], false);
            return false;
        });
        if (!pending.length) { this.clearImageTimer(room.key); return; }
        if (this.waitForImages(room, pending[0]!)) return;
        let slot = this.slots.get(room.key);
        if (!slot) {
            // Reserve capacity atomically, but never hold the pool lock while starting CC.
            slot = await this.poolSerial.run('admission', async () => {
                if (this.closing || !this.store.pending(room.key).length) return undefined;
                const existing = this.slots.get(room.key);
                if (existing) return existing;
                if (this.slots.size >= this.config.runtime.maxWorkers) {
                    const oldest = [...this.slots.values()].filter(s => this.reclaimable(s))
                        .sort((a, b) => a.lastActivity - b.lastActivity)[0];
                    if (!oldest) return undefined;
                    if (!(await this.releaseIdle(oldest))) return undefined;
                }
                const session = this.store.session(room.key);
                const created: Slot = { room, session, abort: new AbortController(), live: true, busy: false,
                    background: -1, pinned: false, lastActivity: Date.now(), deliveries: new Map(), inputGroups: new Map(), acceptedSupplements: new Set(),
                    requests: new Set(), activeRequests: 0, renders: new Map(), replyAnchors: new Map() };
                this.slots.set(room.key, created);
                return created;
            });
            if (!slot) {
                if (this.closing || !this.store.pending(room.key).length) return;
                if (!this.waitingNotified.has(room.key)) {
                    this.waitingNotified.add(room.key);
                    this.notify(room, '当前会话都在工作、监控或停止尚未确认，输入已保存。正常空闲后会自动处理；停止未确认时请维护者检查服务诊断。', pending[0]?.message);
                }
                return;
            }
            this.waitingNotified.delete(room.key);
            const session = slot.session, captured = slot;
            try {
                slot.workspace = await prepareWorkspace(this.config, room, session, this.config.projects.find(p => p.id === room.projectId)!);
                slot.worker = await this.factory(room, session, event => this.event(captured, event), slot.workspace);
            } catch (e) {
                if (!slot.stopUnconfirmed && this.slots.get(room.key) === slot) this.slots.delete(room.key);
                slot.live = false;
                throw e;
            }
            if (!slot.live || this.closing) {
                try { await slot.worker.stop(); slot.stopUnconfirmed = false; }
                catch (error) { slot.stopUnconfirmed = true; throw error; }
                return;
            }
        }
        if (slot.stopUnconfirmed) {
            if (!this.waitingNotified.has(room.key)) {
                this.waitingNotified.add(room.key);
                this.notify(room, '原生进程尚未确认退出；本聊天输入已保存，不会启动可能重叠的新进程。请检查服务诊断。');
            }
            return;
        }
        while (slot.live && !this.closing) {
            // Refresh after every await: arrivals can seal a window or append a control boundary.
            const queued = this.store.pending(room.key), delivery = queued[0];
            if (!delivery) { this.clearImageTimer(room.key); return; }
            if (this.waitForImages(room, delivery)) return;
            const members = imageBatchMembers(queued, delivery);
            if (delivery.sessionId !== slot.session.id) {
                for (const member of members) { this.store.mark(member.uuid, 'cancelled'); this.receipts.settle(room.key, member.sessionId, [member.message], false); }
                continue;
            }
            if (!members.every(member => this.sanitize(room, member))) {
                for (const member of members) { this.store.discardInput(room.key, member.message.id); this.receipts.settle(room.key, member.sessionId, [member.message], false); }
                continue;
            }
            if (delivery.supplementTo && !this.supplementAlive(room, delivery)) {
                this.cancelSupplement(room, delivery, '目标前台运行已经结束或停止。');
                continue;
            }
            if (slot.compacting) return;
            if (delivery.command && (slot.busy || slot.deliveries.size > 0 || slot.activeRequests > 0 || this.approvals.size(slot.session.id))) return;
            const batch = delivery.imageBatch;
            if (batch) this.store.transaction(() => {
                for (const member of members) { member.imageBatch!.closed = true; this.store.queue(member); }
            });
            let handedOff = false, totalBytes = 0;
            const paths: string[] = [];
            const authorized = () => members.every(member => this.inputAuthorized(room, member));
            const stillQueued = () => members.every(member => this.queued(member));
            const checkPreparation = () => {
                if (!slot!.live || slot!.abort.signal.aborted || !authorized() || !stillQueued() || !this.supplementAlive(room, delivery))
                    throw new Error('input preparation cancelled');
            };
            try {
                if (batch && members.reduce((n, d) => n + d.message.resources.length, 0) > Math.min(batch.maxImages, this.config.limits.imageBatchMaxImages))
                    throw new Error('image count limit');
                for (const member of members) {
                    checkPreparation();
                    if (member.message.replyTo && !member.command) {
                        const quoted = await waitOrAbort(() => this.transport.fetch(member.message.replyTo!, room), slot.abort.signal)
                            .catch(error => { if (slot!.abort.signal.aborted) throw error; return undefined; });
                        checkPreparation();
                        if (quoted && readable(room, quoted)) {
                            member.quote = { ...quoted, text: quoted.text.slice(0, this.config.limits.textChars), resources: quoted.resources.slice(0, 12) };
                            this.store.rememberContext(room.key, [member.quote]);
                        }
                    }
                    for (const [index, resource] of member.message.resources.entries()) {
                        checkPreparation();
                        const bytes = await waitOrAbort(() => this.transport.download(member.message.id, resource, slot!.abort.signal), slot.abort.signal);
                        checkPreparation();
                        if (bytes.byteLength > this.config.limits.attachmentBytes) throw new Error('附件超过允许大小');
                        totalBytes += bytes.byteLength;
                        if (batch && totalBytes > Math.min(batch.maxBytes, this.config.limits.imageBatchBytes)) throw new Error('image batch byte limit');
                        const name = basename(resource.name).replace(/[^\p{L}\p{N}_.-]/gu, '_').slice(0, 100) || 'attachment';
                        const path = join(slot.workspace!.inbound, `${hash(member.message.id).slice(0, 16)}-${index}-${name}`);
                        paths.push(path);
                        await writeFile(path, bytes, { mode: 0o600 });
                        member.attachments.push({ path, name: resource.name });
                    }
                }
                checkPreparation();
                // One atomic native-send barrier for every original message in a batch.
                this.store.transaction(() => { for (const member of members) this.store.mark(member.uuid, 'sent'); });
                for (const member of members) {
                    slot.deliveries.set(member.uuid, member);
                    slot.replyAnchors.set(member.uuid, { ...member, message: { ...member.message, text: '', resources: [] }, background: [], quote: undefined, attachments: [] });
                    this.receipts.accept(room, slot.session.id, member.message);
                }
                if (batch) slot.inputGroups.set(delivery.uuid, members.map(d => d.uuid));
                while (slot.replyAnchors.size > 10000) slot.replyAnchors.delete(slot.replyAnchors.keys().next().value!);
                slot.anchor ??= delivery;
                if (delivery.command) slot.compacting = delivery.uuid;
                slot.busy = true;
                slot.lastActivity = Date.now();
                handedOff = true;
                await slot.worker!.send(batch ? { ...delivery, batchInputs: members } : delivery);
                if (delivery.command) return;
            } catch (e) {
                if (!slot.live || slot.abort.signal.aborted || this.closing) return;
                if (!handedOff && !authorized()) {
                    for (const member of members) { this.store.discardInput(room.key, member.message.id); this.receipts.settle(room.key, member.sessionId, [member.message], false); }
                    continue;
                }
                if (!handedOff && delivery.supplementTo && !this.supplementAlive(room, delivery)) {
                    this.cancelSupplement(room, delivery, '目标前台运行已在交付前结束。');
                    continue;
                }
                if (!handedOff && batch) { this.rejectImageBatch(room, members, true); continue; }
                this.store.transaction(() => { for (const member of members) this.store.mark(member.uuid, 'uncertain'); });
                for (const member of members) this.receipts.settle(room.key, member.sessionId, [member.message], false);
                this.notify(room, `本条交付未确认，不会自动重放：${redact(errorText(e), this.secrets)}`, delivery.message);
                if (handedOff) {
                    this.event(slot, { type: 'exit', code: null });
                    await slot.worker?.stop();
                    return;
                }
            } finally {
                if (!handedOff) await Promise.allSettled(paths.map(path => unlink(path)));
            }
        }
    }
    private renderFor(slot: Slot, uuid?: string): Render {
        const human = !!uuid && slot.deliveries.has(uuid);
        const anchor = human ? slot.anchor ?? slot.deliveries.get(uuid!) : uuid ? slot.replyAnchors.get(uuid) : undefined;
        const key = human ? 'human' : 'background:' + (uuid ?? 'unknown');
        let render = slot.renders.get(key);
        if (!render) {
            render = new Render(slot.room, slot.session.id, this.transport, this.store, this.outbox,
                this.config.limits.updateMs, anchor, slot.abort.signal, text => redact(text, this.secrets));
            slot.renders.set(key, render);
        }
        return render;
    }
    private event(slot: Slot, event: WorkerEvent): void {
        if (!slot.live) return;
        slot.lastActivity = Date.now();
        switch (event.type) {
            case 'ready':
                if (!/^[a-f0-9]{8}-(?:[a-f0-9]{4}-){3}[a-f0-9]{12}$/i.test(event.ccId)) break;
                if (slot.session.ccId && slot.session.ccId !== event.ccId) {
                    void this.track(this.stopRoom(slot.room)).then(() => this.notice(slot.room, '原会话恢复返回了不同 ID，已停止，未重置历史。请检查原生会话文件。'));
                    break;
                }
                this.store.setCc(slot.session.id, event.ccId);
                slot.session.ccId = event.ccId;
                break;
            case 'accepted': {
                const delivery = slot.deliveries.get(event.uuid);
                if (!delivery?.supplementTo || slot.acceptedSupplements.has(event.uuid)) break;
                slot.acceptedSupplements.add(event.uuid);
                if (!this.closing && this.inputAuthorized(slot.room, delivery))
                    void this.track(this.notice(slot.room, '补充已交给原生输入流；这不代表模型已读取、理解或执行。', delivery.message));
                break;
            }
            case 'input_rejected': {
                const delivery = slot.deliveries.get(event.uuid);
                if (!delivery?.supplementTo || event.reason !== 'foreground_ended' || slot.acceptedSupplements.has(event.uuid)) break;
                if (!this.cancelSupplement(slot.room, delivery, '原生接收补充前，目标前台运行已结束。', true, 'sent')) break;
                // Persist cancellation and feedback before releasing ownership; a failed transaction remains recoverable.
                slot.deliveries.delete(event.uuid);
                slot.replyAnchors.delete(event.uuid);
                slot.busy = slot.deliveries.size > 0;
                slot.anchor = slot.deliveries.values().next().value;
                this.kickWaiting();
                break;
            }
            case 'consumed':
                this.store.transaction(() => {
                    for (const id of slot.inputGroups.get(event.uuid) ?? [event.uuid])
                        if (slot.deliveries.has(id)) this.store.mark(id, 'consumed');
                });
                break;
            case 'activity':
                if (event.background !== undefined) slot.background = event.background;
                if (event.pinned !== undefined) slot.pinned = event.pinned;
                if (event.state) slot.busy = event.state !== 'idle';
                if (event.tool && /Monitor|CronCreate|ScheduleWakeup/.test(event.tool)) slot.pinned = true;
                if (event.state === 'idle') this.cancelLostSupplements(slot.room);
                this.kickWaiting();
                break;
            case 'text':
            case 'thinking':
                this.renderFor(slot, event.turnId).update(redact(event.text, this.secrets), event.partial, event.type, event.blockId);
                break;
            case 'compaction': {
                const id = slot.compacting ?? slot.anchor?.uuid;
                this.renderFor(slot, id).setStatus(event.phase === 'start' ? '正在整理原生上下文' : '上下文已整理');
                break;
            }
            case 'result': {
                // Actual SDK result IDs deduplicate accounting and terminal rendering across restarts.
                // Result usage remains observable even when subsequent answer persistence fails.
                const disposition = this.store.transaction(() => {
                    const result = recordUsage(this.store, slot.room, slot.session, event);
                    if (result !== 'duplicate') this.store.audit(slot.room.key, 'turn_result', {
                        session: slot.session.id, success: event.success, compact: event.compact,
                        resultId: event.resultId, models: event.models, usage: event.usage ?? {}
                    });
                    return result;
                });
                if (disposition === 'duplicate') break;
                const nativeIds = event.uuids?.length ? event.uuids : event.uuid ? [event.uuid] : [];
                const finished = [...new Set(nativeIds.flatMap(id => slot.inputGroups.get(id) ?? [id]))].filter(id => slot.deliveries.has(id));
                const anchor = finished.length ? slot.deliveries.get(finished[0]!) : event.replyToUuid ? slot.replyAnchors.get(event.replyToUuid) : undefined;
                const origins: Origin[] = finished.length ? finished.map(id => slot.deliveries.get(id)!.message) : anchor ? [anchor.message] : [];
                const key = finished.length ? 'human' : 'background:' + (event.replyToUuid ?? 'unknown');
                const unbound = !slot.renders.has(key) ? slot.renders.get('background:unknown') : undefined;
                const render = unbound ?? this.renderFor(slot, finished[0] ?? event.replyToUuid);
                if (unbound) { unbound.bindOrigin(anchor); slot.renders.delete('background:unknown'); }
                slot.renders.delete(key);
                for (const id of finished) { slot.deliveries.delete(id); slot.acceptedSupplements.delete(id); }
                for (const id of nativeIds) slot.inputGroups.delete(id);
                if (slot.compacting && finished.includes(slot.compacting)) slot.compacting = undefined;
                slot.busy = slot.deliveries.size > 0;
                slot.anchor = slot.deliveries.values().next().value;
                this.cancelLostSupplements(slot.room);
                this.receipts.settle(slot.room.key, slot.session.id, origins, false);
                const compactText = event.compact === 'compacted' ? '上下文已由 Claude Code 原生压缩，会话保持不变。'
                    : event.compact === 'not_needed' ? '原生压缩未执行（例如历史不足），会话保持不变。'
                    : event.compact === 'failed' ? '原生压缩失败，未新建或重置会话。' : undefined;
                const text = compactText ? compactText + (event.text ? '\n\n' + event.text : '') : event.text;
                slot.activeRequests++;
                void this.track(render.finish(redact(text, this.secrets), event.success, this.config.dataDir, origins,
                    event.compact ? '上下文整理' : undefined, () => {
                        for (const id of finished) this.store.mark(id, 'done');
                    })).catch(() => {
                        try { this.store.audit(slot.room.key, 'final_reply_persistence_failed', { session: slot.session.id }); }
                        catch { /* Local storage may itself be unavailable. */ }
                        const current = !this.closing ? roomFromKey(this.config, slot.room.key) : undefined;
                        if (current) void this.track(this.notice(current,
                            '回复保存失败，本轮不标记完成；未确认输入不会自动重放。请检查服务诊断和磁盘状态。')).catch(() => {});
                    }).finally(() => { slot.activeRequests--; this.kickWaiting(); });
                this.kickWaiting();
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
                    slot.activeRequests++;
                    void this.track(this.request(slot, event)).then(value => { if (slot.live)
                        slot.worker?.respond(event.id, value); }).catch(e => { if (slot.live)
                        slot.worker?.respond(event.id, { approved: false, ok: false, error: redact(errorText(e), this.secrets) }); }).finally(() => { slot.activeRequests--; this.kickWaiting(); });
                }
                break;
            case 'error':
                void this.notice(slot.room, redact(event.message, this.secrets), slot.anchor?.message);
                break;
            case 'exit': {
                const cleanupFailed = event.cleanupConfirmed === false;
                if (cleanupFailed) slot.stopUnconfirmed = true;
                const interrupted = slot.deliveries.size > 0 || slot.background > 0 || slot.pinned;
                const pendingApproval = this.approvals.size(slot.session.id) > 0;
                const current = !this.closing ? roomFromKey(this.config, slot.room.key) : undefined;
                slot.live = false;
                let noticeId: string | undefined;
                this.store.transaction(() => {
                    slot.abort.abort();
                    this.approvals.cancelSession(slot.session.id);
                    this.receipts.cancelSession(slot.session.id);
                    const uncertain = this.store.db.prepare("UPDATE inbox SET status='uncertain' WHERE session=? AND status IN ('sent','consumed')").run(slot.session.id).changes;
                    if (cleanupFailed) this.store.audit(slot.room.key, 'worker_descendants_unconfirmed', { session: slot.session.id });
                    if (current && (cleanupFailed || interrupted || pendingApproval || uncertain)) {
                        const text = cleanupFailed
                            ? '原生进程异常退出，但后代清理未确认。本聊天已暂停；不会启动替代进程或重放未确认操作。请维护者核实进程状态。'
                            : '原生工作进程意外退出。当前审批／追问已取消；未确认的输入与外部操作不会自动重放。请先核实结果。会话记录仍保留，可以继续原会话。';
                        noticeId = this.outbox.saveRecovery(current, slot.session.id, text, displayCard('工作中断', text));
                    }
                });
                this.cancelLostSupplements(slot.room);
                for (const render of slot.renders.values()) void this.track(render.cancel('执行中断'));
                if (!cleanupFailed && this.slots.get(slot.room.key) === slot) this.slots.delete(slot.room.key);
                if (current && noticeId) void this.track(this.outbox.deliver(noticeId, current)).catch(() => {});
                if (!this.closing && !cleanupFailed)
                    for (const room of this.rooms.values())
                        if (room.key !== slot.room.key || this.store.pending(room.key).some(d => !!d.imageBatch)) this.kick(room);
                break;
            }
        }
    }
    private async request(slot: Slot, event: Extract<WorkerEvent, { type: 'request' }>): Promise<Args> {
        const current = roomFromKey(this.config, slot.room.key);
        if (!current || slot.abort.signal.aborted) throw new Error('Conversation is no longer authorized');
        const project = this.config.projects.find(p => p.id === slot.room.projectId)!;
        if (event.kind === 'history') return waitOrAbort(
            () => this.history.read(current, slot.session.id, event.input, slot.abort.signal), slot.abort.signal
        );
        if (event.kind === 'action') {
            const w = slot.workspace!;
            return this.broker.execute(slot.room, slot.session.id, project, event.name, event.input, slot.abort.signal, w.work, path => translateWorkerFile(this.config, w, path));
        }
        if (event.kind === 'attachment') {
            const id = String(event.input.messageId ?? ''), key = String(event.input.key ?? '');
            const message = this.store.contextMessage(slot.room.key, id);
            if (!message || !readable(current, message)) throw new Error('attachment message is not authorized in this conversation');
            const resource = message.resources?.find(r => r.key === key);
            if (!resource) throw new Error('unknown attachment');
            const data = await waitOrAbort(() => this.transport.download(id, resource, slot.abort.signal), slot.abort.signal);
            if (data.byteLength > this.config.limits.attachmentBytes) throw new Error('attachment exceeds size limit');
            const w = slot.workspace!;
            const name = `${hash(id + key).slice(0, 24)}-${basename(resource.name).replace(/[^\p{L}\p{N}_.-]/gu, '_').slice(0, 100)}`;
            const path = join(w.inbound, name);
            await writeFile(path, data, { mode: 0o600 });
            return { ok: true, path };
        }
        if (event.kind === 'send_file') {
            const w = slot.workspace!;
            const translated = translateWorkerFile(this.config, w, String(event.input.path ?? ''));
            const path = await safeFile(w.work, translated);
            if ((await stat(path)).size > this.config.limits.attachmentBytes) throw new Error('file exceeds send limit');
            const bytes = await readBoundedFile(w.work, path, this.config.limits.attachmentBytes);
            const dir = join(this.config.dataDir, 'exports', hash(slot.room.key).slice(0, 32));
            await mkdir(dir, { recursive: true, mode: 0o700 });
            const snapshot = join(dir, randomUUID());
            let recorded = false;
            try {
                await writeFile(snapshot, bytes, { mode: 0o600 });
                if (slot.abort.signal.aborted) throw new Error('operation cancelled');
                const name = basename(String(event.input.name ?? basename(path)));
                const origins = [...slot.deliveries.values()].map(d => d.message);
                const id = this.outbox.saveFile(current, slot.session.id, snapshot, name, origins, slot.anchor?.message.id);
                recorded = true;
                if (slot.abort.signal.aborted) {
                    this.store.db.prepare("UPDATE outbox SET state='cancelled' WHERE id=?").run(id);
                    throw new Error('operation cancelled');
                }
                const sent = await this.outbox.deliver(id, current);
                if (!sent) return { ok: false, deliveryId: id,
                    error: 'File not confirmed delivered. Check /outputs; an explicit /retry-output may be required.' };
                const row = this.store.db.prepare('SELECT file_target FROM outbox WHERE id=?').get(id);
                return { ok: true, deliveryId: id, messageId: String(row?.file_target ?? '') };
            } finally {
                if (!recorded) await unlink(snapshot).catch(error => {
                    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') this.store.audit(slot.room.key, 'untracked_export_cleanup_failed', {});
                });
            }
        }
        const questions = event.kind === 'question' ? event.input.questions as Question[] : undefined;
        return this.approvals.request(slot.room, slot.session.id, event.name, event.input, `工具：${event.name}\n参数：\`${redact(JSON.stringify(event.input), this.secrets).slice(0, 5000)}\``, slot.abort.signal, questions);
    }
    private async command(room: Room, message: Message): Promise<void> {
        const [command = '', ...parts] = message.text.trim().split(/\s+/), arg = parts.join(' ');
        const maintainer = room.maintainers.includes(message.senderId);
        const slot = this.slots.get(room.key);
        const view = managementReply(command, arg, { room, actor: message.senderId, store: this.store,
            runtime: slot, approvals: slot ? this.approvals.size(slot.session.id) : 0 });
        if (view !== undefined) {
            await this.notice(room, view, message);
            return;
        }
        if (command === '/retry-output') {
            if (!maintainer && room.dmUser !== message.senderId) {
                await this.notice(room, '需要维护者批准补发。', message);
                return;
            }
            const ok = await this.outbox.deliver(arg, room, true);
            await this.notice(room, ok ? '已补发。此前结果不明确时可能出现重复消息。' : '未补发：记录不存在、已发送或渠道仍不可用。', message);
            return;
        }
        if (command === '/answer') {
            const id = parts.shift() ?? '';
            // Question settlement is synchronous; an informational Feishu acknowledgement
            // must not hold the per-room inbound lock and block a later /stop.
            this.notify(room, this.approvals.answer(room, message.senderId, id, parts.join(' ')), message);
            return;
        }
        if (!maintainer && room.dmUser !== message.senderId) {
            await this.notice(room, '此操作需要项目维护者。', message);
            return;
        }
        const stopConfirmed = async (): Promise<boolean> => {
            try { await this.stopRoom(room); return true; }
            catch {
                await this.notice(room, '原生进程停止失败，尚未确认退出。当前会话保留，不能启动替代进程或切换会话；请先检查服务诊断并核实进程。', message);
                return false;
            }
        };
        if (command === '/resume') {
            if (!this.store.db.prepare('SELECT 1 FROM sessions WHERE room=? AND id=?').get(room.key, arg)) {
                await this.notice(room, '只能恢复本聊天列出的会话。', message);
                return;
            }
            if (!(await stopConfirmed())) return;
            this.store.resume(room.key, arg);
            await this.notice(room, '已选择历史会话，旧运行未交付的补充已取消。下一条消息会恢复上下文；旧后台进程不会自动重启。', message);
            return;
        }
        if (command === '/stop' || command === '/new') {
            if (!(await stopConfirmed())) return;
            if (command === '/new') {
                this.store.db.prepare("DELETE FROM inbox WHERE room=? AND status='background'").run(room.key);
                this.store.newSession(room.key);
                await this.notice(room, '已新建本聊天会话，旧运行未交付的补充已取消。', message);
            } else await this.notice(room, '已停止本聊天运行、后台进程和待交付输入；未交付的补充已取消。上下文和会话 ID 保留，下条消息继续原会话；只有 /new 新建会话。', message);
        }
    }
    private async stopRoom(room: Room): Promise<void> {
        this.clearImageTimer(room.key);
        const slot = this.slots.get(room.key);
        if (slot?.stopUnconfirmed && !slot.worker) throw new Error('Native worker termination remains unconfirmed');
        const session = slot?.session ?? this.store.listSessions(room.key).find(s => s.state === 'open');
        if (session) {
            this.approvals.cancelSession(session.id);
            this.receipts.cancelSession(session.id);
            this.store.stopSession(session.id);
            this.store.db.prepare("UPDATE outbox SET state='cancelled' WHERE session=? AND state IN ('pending','uncertain','sending','preparing')").run(session.id);
        }
        if (slot) { slot.live = false; slot.abort.abort(); }
        await this.dispatchSerial.run(room.key, async () => {
            if (!slot) return;
            try { await slot.worker?.stop(); }
            catch (error) {
                slot.stopUnconfirmed = true;
                this.store.audit(room.key, 'worker_stop_unconfirmed', { session: slot.session.id, error: redact(errorText(error), this.secrets) });
                throw error;
            } finally { await Promise.allSettled([...slot.renders.values()].map(r => r.cancel())); }
            if (this.slots.get(room.key) === slot) this.slots.delete(room.key);
        });
        this.kickWaiting();
    }
    /** Informational sends cannot hold inbound chat locks while Feishu is slow. */
    private notify(room: Room, text: string, message?: Message): void {
        void this.track(this.notice(room, text, message)).catch(() => {});
    }
    private async notice(room: Room, text: string, message?: Message): Promise<void> {
        if (this.closing) return;
        try {
            // Bind a best-effort reply to the session at its origin. A delayed
            // Feishu response must not be indexed against a later /new or /resume,
            // nor retroactively acquire a session that did not yet exist.
            const sessionId = this.store.listSessions(room.key).find(s => s.state === 'open')?.id;
            // These best-effort replies are not durable outbox operations. Stop
            // awaiting their network outcome when shutdown begins; the underlying
            // transport may still finish remotely, but cannot access closed state.
            const id = await waitOrAbort(
                () => this.transport.send(room, text, message?.id, message?.threadId), this.noticeAbort.signal);
            if (this.closing) return;
            if (sessionId) this.store.recordOutput(id, room.key, sessionId);
        } catch {
            if (!this.closing) this.store.audit(room.key, 'notice_failed', {});
        }
    }
    async sweep(now = Date.now()): Promise<void> {
        if (this.closing) return;
        await this.outbox.pruneCompletedExports(now, join(this.config.dataDir, 'exports'));
        this.store.trim(now, this.config.limits.backgroundMs);
        this.receipts.retry();
        for (const room of this.rooms.values()) for (const id of this.outbox.retryable(room.key)) await this.outbox.deliver(id, room);
        for (const [key, values] of this.rates) {
            const alive = values.filter(t => t > now - 60000);
            if (alive.length) this.rates.set(key, alive); else this.rates.delete(key);
        }
        await this.poolSerial.run('admission', async () => {
            if (this.closing || !this.config.runtime.idleMs) return;
            for (const slot of this.slots.values())
                if (this.reclaimable(slot) && now - slot.lastActivity > this.config.runtime.idleMs) await this.releaseIdle(slot);
        });
        this.kickWaiting();
    }
    private reclaimable(slot: Slot): boolean {
        return slot.live && !!slot.worker && !slot.busy && !slot.pinned && slot.background === 0 &&
            slot.activeRequests === 0 && slot.deliveries.size === 0 &&
            !this.store.countPending(slot.room.key) && !this.approvals.size(slot.session.id);
    }
    private async releaseIdle(slot: Slot): Promise<boolean> {
        slot.live = false;
        slot.abort.abort();
        try { await slot.worker!.stop(); }
        catch (error) {
            slot.stopUnconfirmed = true;
            this.store.audit(slot.room.key, 'idle_process_stop_unconfirmed', { session: slot.session.id, error: redact(errorText(error), this.secrets) });
            const current = roomFromKey(this.config, slot.room.key);
            if (current) void this.track(this.notice(current,
                '闲置进程停止失败，尚未确认退出；已保留进程名额以避免并行执行。请检查服务诊断。')).catch(() => {});
            return false;
        }
        if (this.slots.get(slot.room.key) === slot) this.slots.delete(slot.room.key);
        this.store.audit(slot.room.key, 'idle_process_released', { session: slot.session.id });
        return true;
    }
    private kickWaiting(): void {
        if (this.closing) return;
        for (const key of this.store.pendingRooms()) {
            const room = this.rooms.get(key);
            if (room) this.kick(room);
        }
    }
    async close(): Promise<void> {
        this.closing = true;
        this.noticeAbort.abort();
        this.outbox.close(); // Release stalled durable Feishu waits before draining tracked tasks.
        clearInterval(this.housekeeping);
        for (const key of this.imageTimers.keys()) this.clearImageTimer(key);
        this.approvals.close();
        await Promise.allSettled([...this.slots.values()].map(async (slot) => {
            slot.live = false;
            slot.abort.abort();
            this.receipts.cancelSession(slot.session.id);
            try {
                if (slot.worker) { await slot.worker.stop(); slot.stopUnconfirmed = false; }
            } catch (error) {
                slot.stopUnconfirmed = true;
                this.store.audit(slot.room.key, 'worker_stop_unconfirmed', { session: slot.session.id, error: redact(errorText(error), this.secrets) });
            } finally { await Promise.allSettled([...slot.renders.values()].map(r => r.cancel())); }
        }));
        await this.inputSerial.drain();
        await this.dispatchSerial.drain();
        await this.poolSerial.drain();
        await Promise.allSettled([...this.tasks]);
        await this.receipts.close();
        for (const [key, slot] of this.slots) if (!slot.stopUnconfirmed) this.slots.delete(key);
        if (this.slots.size) throw new Error(`Native worker termination unconfirmed (${this.slots.size}). Verify remaining native processes before restarting.`);
    }
}
