import type { Args, WorkerEvent } from './types.ts';

/** SDK attribution is stamped on selected frames, not every token. Retain it until the result. */
export class NativeOutput {
    private currentMessage = '';
    private sequence = 0;
    private turnAnchor?: string;
    private humanAnchor?: string;
    private taskAnchors = new Map<string, string>();
    private blocks = new Map<string, { index: number; kind: string; matched: boolean }[]>();
    readonly emit: (event: WorkerEvent) => void;
    constructor(emit: (event: WorkerEvent) => void) { this.emit = emit; }
    private stamp(event: Args): string | undefined {
        const origin = event.origin as Args | undefined;
        if (origin?.kind === 'task-notification' && typeof origin.runId === 'string') return this.taskAnchors.get(origin.runId);
        if (typeof origin?.senderTaskId === 'string') return this.taskAnchors.get(origin.senderTaskId);
        const ids = Array.isArray(event.user_message_uuids) && event.user_message_uuids.length ? event.user_message_uuids : [event.user_message_uuid];
        return ids.find((id): id is string => typeof id === 'string');
    }
    origin(event: Args): string | undefined { return this.stamp(event) ?? this.turnAnchor; }
    endTurn(): void { this.turnAnchor = undefined; this.currentMessage = ''; this.blocks.clear(); }
    handle(event: Args): void {
        if (event.parent_tool_use_id) return;
        const stamp = this.stamp(event);
        if (stamp && ['assistant', 'stream_event', 'result'].includes(String(event.type))) {
            this.turnAnchor = stamp;
            if (!event.origin || (event.origin as Args).kind === 'human') this.humanAnchor = stamp;
        }
        if (event.type === 'system' && event.subtype === 'task_started') {
            const anchor = typeof event.parent_task_id === 'string' ? this.taskAnchors.get(event.parent_task_id) : this.turnAnchor ?? this.humanAnchor;
            if (anchor) for (const id of [event.task_id, event.run_id]) if (typeof id === 'string') this.taskAnchors.set(id, anchor);
            if (this.taskAnchors.size > 10000) this.taskAnchors.delete(this.taskAnchors.keys().next().value!);
        }
        const turnId = this.origin(event);
        const send = (kind: string, text: unknown, partial: boolean, id: string) => {
            if ((kind === 'text' || kind === 'thinking') && typeof text === 'string')
                this.emit({ type: kind, text, partial, blockId: id, turnId });
        };
        if (event.type === 'stream_event') {
            const data = event.event as Args | undefined;
            if (!data) return;
            if (data.type === 'message_start') {
                const message = data.message as Args | undefined;
                this.currentMessage = typeof message?.id === 'string' ? message.id : `stream-${++this.sequence}`;
                if (!this.blocks.has(this.currentMessage)) this.blocks.set(this.currentMessage, []);
            }
            const index = Number(data.index ?? 0), id = `${this.currentMessage}:${index}`;
            if (data.type === 'content_block_start') {
                const block = data.content_block as Args | undefined;
                if (block) {
                    const known = this.blocks.get(this.currentMessage) ?? [];
                    if (!known.some(b => b.index === index)) known.push({ index, kind: String(block.type), matched: false });
                    this.blocks.set(this.currentMessage, known);
                    send(String(block.type), block.type === 'thinking' ? block.thinking : block.text, false, id);
                }
            }
            if (data.type === 'content_block_delta') {
                const delta = data.delta as Args | undefined;
                if (delta?.type === 'text_delta') send('text', delta.text, true, id);
                if (delta?.type === 'thinking_delta') send('thinking', delta.thinking, true, id);
            }
        } else if (event.type === 'assistant') {
            const message = event.message as Args | undefined;
            if (!Array.isArray(message?.content)) return;
            const id = typeof message.id === 'string' ? message.id : this.currentMessage || `message-${++this.sequence}`;
            const known = this.blocks.get(id) ?? [];
            // Streaming CLI messages often carry ONE completed block, even if its original index is >0.
            message.content.forEach((block: Args, index: number) => {
                let matching = message.content instanceof Array && message.content.length > 1
                    ? known.find(b => b.index === index && b.kind === block.type) : undefined;
                matching ??= known.find(b => !b.matched && b.kind === block.type);
                matching ??= known.filter(b => b.kind === block.type).at(-1);
                if (matching) matching.matched = true;
                send(String(block.type), block.type === 'thinking' ? block.thinking : block.text, false, `${id}:${matching?.index ?? index}`);
            });
        }
        if (this.blocks.size > 256) this.blocks.delete(this.blocks.keys().next().value!);
    }
}
