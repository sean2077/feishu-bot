import { randomUUID } from 'node:crypto';
import { hash } from './util.ts';
import type { Args, Room, Transport } from './types.ts';
import { Store } from './store.ts';
import { authorizedAuthor } from './policy.ts';
export interface Question {
    question: string;
    header?: string;
    options: {
        label: string;
        description?: string;
    }[];
    multiSelect?: boolean;
}
type Outcome = {
    approved: boolean;
    answers?: Record<string, string>;
    reason?: string;
};
interface Pending {
    room: Room;
    session: string;
    kind: 'permission' | 'question';
    expires: number;
    messageId?: string;
    name: string;
    summary: string;
    cards: Map<string, string | undefined>;
    forwarded: boolean;
    questions: Question[];
    selected: Map<string, Map<number, Set<number>>>;
    resolve: (value: Outcome) => void;
    timer: ReturnType<typeof setTimeout>;
    detach: () => void;
}
export function displayCard(title: string, text: string, buttons: object[] = []): object {
    return { schema: '2.0', config: { wide_screen_mode: true }, header: { title: { tag: 'plain_text', content: title } }, body: { elements: [{ tag: 'markdown', content: text }, ...buttons] } };
}
function button(label: string, value: Args, style = 'default'): object { return { tag: 'button', text: { tag: 'plain_text', content: label.slice(0, 80) }, type: style, value }; }
export class Approvals {
    private pending = new Map<string, Pending>();
    readonly store: Store;
    readonly transport: Transport;
    readonly ttl: number;
    readonly resolveRoom?: (key: string) => Room | undefined;
    constructor(store: Store, transport: Transport, ttl: number, resolveRoom?: (key: string) => Room | undefined) { this.store = store; this.transport = transport; this.ttl = ttl; this.resolveRoom = resolveRoom; }
    async request(room: Room, session: string, name: string, input: Args, summary: string, signal?: AbortSignal, questions?: Question[]): Promise<Outcome> {
        if (signal?.aborted)
            return { approved: false, reason: 'cancelled' };
        if (questions && (!questions.length || questions.length > 8 || questions.some(q => !q || typeof q.question !== 'string' || !q.question || !Array.isArray(q.options) || q.options.length > 12 || q.options.some(o => !o || typeof o.label !== 'string'))))
            return { approved: false, reason: 'unsupported question shape' };
        if (this.pending.size >= 256)
            return { approved: false, reason: 'too many pending requests' };
        const id = randomUUID(), expires = Date.now() + this.ttl, kind = questions ? 'question' : 'permission';
        this.store.db.prepare('INSERT INTO requests(id,room,session,kind,digest,state,expires) VALUES(?,?,?,?,?,?,?)').run(id, room.key, session, kind, hash(JSON.stringify({ name, input })), 'pending', expires);
        return new Promise<Outcome>((resolve) => {
            const cancel = () => this.settle(id, { approved: false, reason: 'cancelled' });
            const timer = setTimeout(() => this.settle(id, { approved: false, reason: 'expired' }), this.ttl);
            timer.unref();
            signal?.addEventListener('abort', cancel, { once: true });
            const p: Pending = { room, session, kind, expires, name, summary, cards: new Map(), forwarded: kind === 'permission' && !!room.dmUser && !room.maintainers.includes(room.dmUser), questions: questions ?? [], selected: new Map(), resolve, timer, detach: () => signal?.removeEventListener('abort', cancel) };
            this.pending.set(id, p);
            const targets = p.forwarded ? room.maintainers.map(user => ({ ...room, chatId: user, dmUser: user })) : [room];
            if (p.forwarded)
                void this.transport.send(room, '这项操作需要项目维护者批准。已向维护者私聊发送操作摘要与参数；不会转发聊天历史。').catch(() => { });
            void Promise.all(targets.map(async (target) => {
                try {
                    const messageId = await this.transport.card(target, this.render(id, p));
                    p.cards.set(messageId, p.forwarded ? target.dmUser : undefined);
                    p.messageId ??= messageId;
                    if (!p.forwarded)
                        this.store.recordOutput(messageId, room.key, session);
                    if (!this.pending.has(id))
                        void this.transport.update(messageId, displayCard('请求已结束', '该请求不再接受操作。')).catch(() => { });
                    return true;
                }
                catch {
                    return false;
                }
            })).then(results => { if (!results.some(Boolean))
                this.settle(id, { approved: false, reason: 'card delivery failed' }); });
            if (signal?.aborted)
                cancel();
        });
    }
    private render(id: string, p: Pending): object {
        if (p.kind === 'permission')
            return displayCard('需要维护者批准', `${p.summary}\n\n仅批准这一次操作，超时或停止后失效。\n请求：\`${id}\``, [
                button('批准一次', { request: id, decision: 'allow' }, 'primary'), button('拒绝', { request: id, decision: 'deny' }, 'danger')
            ]);
        const buttons: object[] = [];
        p.questions.forEach((q, qi) => q.options.forEach((o, oi) => buttons.push(button(`${qi + 1}. ${o.label}`, { request: id, question: qi, option: oi }))));
        buttons.push(button('提交选择', { request: id, decision: 'submit' }, 'primary'), button('取消', { request: id, decision: 'deny' }));
        return displayCard('需要补充信息', p.questions.map((q, i) => `${i + 1}. ${q.question}`).join('\n\n') + `\n\n可多题选择后提交。自由回答：\`/answer ${id} 你的回答\`。`, buttons);
    }
    async click(chatId: string, operator: string, value: unknown, messageId?: string): Promise<string> {
        if (!value || typeof value !== 'object')
            return '';
        const v = value as Args;
        const id = typeof v.request === 'string' ? v.request : '';
        const p = this.pending.get(id);
        if (!p) {
            const prior = this.store.db.prepare('SELECT room FROM requests WHERE id=?').get(id);
            const room = prior && this.resolveRoom?.(String(prior.room));
            return room && room.chatId === chatId && authorizedAuthor(room, operator) ? '请求已结束或过期' : '';
        }
        const current = this.resolveRoom ? this.resolveRoom(p.room.key) : p.room;
        if (!current) return '';
        p.room = current;
        if (p.forwarded) {
            // A forwarded copy has a different message ID. The callback must name the
            // original card delivered to this exact maintainer, never a caller-supplied chat.
            if (!messageId || p.cards.get(messageId) !== operator)
                return '';
        }
        else {
            if (p.room.chatId !== chatId)
                return '';
            if (messageId && !p.cards.has(messageId))
                return '';
            if (p.room.dmUser && p.room.dmUser !== operator)
                return '';
            if (!authorizedAuthor(p.room, operator)) return '';
        }
        if (p.expires <= Date.now()) return '请求已结束或过期';
        if (p.kind === 'permission' && !p.room.maintainers.includes(operator))
            return '仅维护者可以审批';
        if (v.decision === 'deny') {
            this.settle(id, { approved: false, reason: 'denied' }, operator);
            return '已拒绝';
        }
        if (p.kind === 'permission') {
            if (v.decision !== 'allow')
                return '无效决定';
            this.settle(id, { approved: true }, operator);
            return '已批准一次';
        }
        if (v.decision === 'submit') {
            const answers: Record<string, string> = {};
            for (let qi = 0; qi < p.questions.length; qi++) {
                const q = p.questions[qi]!, selected = p.selected.get(operator)?.get(qi);
                if (!selected?.size)
                    return '请回答所有问题';
                answers[q.question] = [...selected].map(i => q.options[i]!.label).join(', ');
            }
            this.settle(id, { approved: true, answers }, operator);
            return '已提交';
        }
        const qi = Number(v.question), oi = Number(v.option), q = p.questions[qi];
        if (!Number.isInteger(qi) || !Number.isInteger(oi) || !q?.options[oi])
            return '无效选项';
        const choices = p.selected.get(operator) ?? new Map<number, Set<number>>();
        const selected = choices.get(qi) ?? new Set<number>();
        if (!q.multiSelect)
            selected.clear();
        if (selected.has(oi))
            selected.delete(oi);
        else
            selected.add(oi);
        // Do not render one member's choices onto a card shared by the group.
        // Card-action toasts confirm the current operator's private selection.
        choices.set(qi, selected);
        p.selected.set(operator, choices);
        const labels = [...selected].map(i => q.options[i]!.label).join(', ') || '未选择';
        return `已记录你的选择：${q.question.slice(0, 50)} → ${labels.slice(0, 80)}`;
    }
    answer(room: Room, operator: string, id: string, text: string): string {
        const p = this.pending.get(id);
        if (!p || p.kind !== 'question' || p.room.key !== room.key || p.expires <= Date.now())
            return '问题不存在或已过期';
        const current = this.resolveRoom ? this.resolveRoom(p.room.key) : p.room;
        if (!current || !authorizedAuthor(current, operator)) return '';
        if (!text.trim())
            return '回答不能为空';
        if (p.questions.length !== 1) {
            try {
                const answers = JSON.parse(text) as Record<string, unknown>;
                if (!answers || p.questions.some(q => typeof answers[q.question] !== 'string') || Object.keys(answers).length !== p.questions.length)
                    throw new Error('incomplete');
                this.settle(id, { approved: true, answers: answers as Record<string, string> }, operator);
                return '回答已提交';
            }
            catch {
                return '多题请使用卡片；或使用 JSON：{"问题原文":"回答"}';
            }
        }
        this.settle(id, { approved: true, answers: { [p.questions[0]!.question]: text } }, operator);
        return '回答已提交';
    }
    private settle(id: string, value: Outcome, actor = 'system'): void {
        const p = this.pending.get(id);
        if (!p)
            return;
        const state = value.approved ? 'approved' : value.reason === 'expired' ? 'expired' : 'denied';
        const changed = this.store.db.prepare("UPDATE requests SET state=?,actor=? WHERE id=? AND state='pending'").run(state, actor, id).changes;
        if (!changed)
            return;
        this.pending.delete(id);
        clearTimeout(p.timer);
        p.detach();
        this.store.audit(p.room.key, 'request_settled', { id, kind: p.kind, state, actor });
        p.resolve(value);
        for (const messageId of p.cards.keys())
            void this.transport.update(messageId, displayCard(value.approved ? '已确认' : '请求已结束', value.approved ? '本次请求已确认，不再接受重复操作。' : `未执行：${value.reason ?? 'denied'}`)).catch(() => { });
    }
    cancelSession(session: string): void { for (const [id, p] of this.pending)
        if (p.session === session)
            this.settle(id, { approved: false, reason: 'cancelled' }); }
    size(session: string): number { return [...this.pending.values()].filter(p => p.session === session).length; }
    close(): void { for (const id of [...this.pending.keys()])
        this.settle(id, { approved: false, reason: 'shutdown' }); }
}
