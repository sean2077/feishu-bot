import { randomUUID } from 'node:crypto';
import type { Store } from './store.ts';
import type { Config, Delivery, Message } from './types.ts';

export function imageBatchEligible(message: Message): boolean {
    return message.imageOnly === true && !message.inputIssue && message.resources.length > 0 &&
        message.resources.every(r => r.kind === 'image') && !message.text.startsWith('/');
}
function compatible(a: Delivery, b: Delivery): boolean {
    return a.roomKey === b.roomKey && a.sessionId === b.sessionId && a.message.senderId === b.message.senderId &&
        a.message.replyTo === b.message.replyTo && a.message.threadId === b.message.threadId;
}
/** Call inside the input transaction. Sealing never moves or replaces original rows. */
export function sealImageWindows(store: Store, room: string): boolean {
    let changed = false;
    for (const delivery of store.pending(room)) {
        if (delivery.imageBatch && !delivery.imageBatch.closed) {
            delivery.imageBatch.closed = true;
            store.queue(delivery);
            changed = true;
        }
    }
    return changed;
}
/** Called before inserting this delivery, in the same SQLite transaction as its original input. */
export function stageImageBatch(store: Store, delivery: Delivery, limits: Config['limits'], now: number): void {
    if (!limits.imageBatchMs || delivery.command || delivery.supplementTo || !imageBatchEligible(delivery.message)) {
        sealImageWindows(store, delivery.roomKey);
        return;
    }
    const pending = store.pending(delivery.roomKey), tail = pending.at(-1);
    const previous = tail?.imageBatch;
    const members = previous ? pending.filter(d => d.imageBatch?.id === previous.id) : [];
    const images = members.reduce((n, d) => n + d.message.resources.length, 0) + delivery.message.resources.length;
    if (tail && previous && !previous.closed && previous.deadline > now && compatible(tail, delivery) &&
        members.every(d => compatible(d, delivery) && !d.command && !d.supplementTo) &&
        images <= Math.min(previous.maxImages, limits.imageBatchMaxImages)) {
        delivery.imageBatch = { ...previous, maxBytes: Math.min(previous.maxBytes, limits.imageBatchBytes) };
        if (images >= Math.min(previous.maxImages, limits.imageBatchMaxImages)) delivery.imageBatch.closed = true;
        for (const member of members) { member.imageBatch = { ...delivery.imageBatch }; store.queue(member); }
    } else {
        sealImageWindows(store, delivery.roomKey);
        delivery.imageBatch = { id: randomUUID(), deadline: now + limits.imageBatchMs,
            closed: delivery.message.resources.length >= limits.imageBatchMaxImages,
            maxImages: limits.imageBatchMaxImages, maxBytes: limits.imageBatchBytes };
    }
}
/** Only consecutive rows with the same persisted group can form a wire input. */
export function imageBatchMembers(pending: Delivery[], first: Delivery): Delivery[] {
    if (!first.imageBatch) return [first];
    const start = pending.findIndex(d => d.uuid === first.uuid), out: Delivery[] = [];
    for (const delivery of pending.slice(start)) {
        if (delivery.imageBatch?.id !== first.imageBatch.id) break;
        if (!compatible(delivery, first) || delivery.command || delivery.supplementTo || !imageBatchEligible(delivery.message))
            throw new Error('Invalid persisted image batch');
        out.push(delivery);
    }
    return out;
}
