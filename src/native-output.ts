import type { Args, WorkerEvent } from './types.ts';

/** SDK attribution is stamped on selected frames, not every token. Retain it until the result. */
export class NativeOutput {
    private currentMessage = '';
    private sequence = 0;
    private turnAnchor?: string;
    private turnSource: 'foreground' | 'background' | 'unattributed' = 'unattributed';
    private turnBackgroundId?: string;
    private taskAnchors = new Map<string, string>();
    private blocks = new Map<string, { index: number; kind: string; matched: boolean }[]>();
    readonly emit: (event: WorkerEvent) => void;
    private readonly isHumanInput: (id: string) => boolean;
    constructor(emit: (event: WorkerEvent) => void, isHumanInput: (id: string) => boolean = () => true) { this.emit = emit; this.isHumanInput = isHumanInput; }
    private stamp(event: Args): string | undefined {
        const origin = event.origin as Args | undefined;
        if (origin?.kind === 'task-notification' && typeof origin.runId === 'string') return this.taskAnchors.get(origin.runId);
        if (typeof origin?.senderTaskId === 'string') return this.taskAnchors.get(origin.senderTaskId);
        if (origin && origin.kind !== 'human') return undefined;
        const ids = Array.isArray(event.user_message_uuids) && event.user_message_uuids.length ? event.user_message_uuids
            : [event.user_message_uuid, ...(event.type === 'user' ? [event.uuid] : [])];
        return ids.find((id): id is string => typeof id === 'string' && this.isHumanInput(id));
    }
    origin(event: Args): string | undefined {
        const origin = event.origin as Args | undefined;
        const stamp = this.stamp(event);
        // Unknown background origins must never borrow a foreground anchor.
        return origin && origin.kind !== 'human' ? stamp : stamp ?? this.turnAnchor;
    }
    source(event: Args): 'foreground' | 'background' | 'unattributed' {
        const origin = event.origin as Args | undefined;
        if (origin && (origin.kind !== 'human' || typeof origin.senderTaskId === 'string')) return 'background';
        if (this.stamp(event) || origin?.kind === 'human') return 'foreground';
        return this.turnSource;
    }
    backgroundId(event: Args): string | undefined {
        const origin = event.origin as Args | undefined;
        if (origin) {
            if (typeof origin.runId === 'string' && origin.runId) return origin.runId;
            if (typeof origin.senderTaskId === 'string' && origin.senderTaskId) return origin.senderTaskId;
            return undefined;
        }
        return this.turnBackgroundId;
    }
    endTurn(): void { this.turnAnchor = undefined; this.turnSource = 'unattributed'; this.turnBackgroundId = undefined; this.currentMessage = ''; this.blocks.clear(); }
    handle(event: Args): void {
        if (event.parent_tool_use_id) return;
        const stamp = this.stamp(event);
        // Native origins are declared on user trigger/result frames; assistant
        // and token frames often inherit them without repeating any metadata.
        // A synthetic tool_result user frame without origin is not a new turn.
        if (['assistant', 'stream_event', 'result'].includes(String(event.type)) || (event.type === 'user' && (event.origin || stamp))) {
            this.turnSource = this.source(event);
            this.turnBackgroundId = this.turnSource === 'background' ? this.backgroundId(event) : undefined;
            if (stamp) this.turnAnchor = stamp;
            else if (event.origin && (event.origin as Args).kind !== 'human') this.turnAnchor = undefined;
        }
        if (event.type === 'system' && event.subtype === 'task_started') {
            const anchor = typeof event.parent_task_id === 'string' ? this.taskAnchors.get(event.parent_task_id) : this.turnAnchor;
            if (anchor) for (const id of [event.task_id, event.run_id]) if (typeof id === 'string') this.taskAnchors.set(id, anchor);
            while (this.taskAnchors.size > 10000) this.taskAnchors.delete(this.taskAnchors.keys().next().value!);
        }
        const turnId = this.origin(event);
        const send = (kind: string, text: unknown, partial: boolean, id: string) => {
            if ((kind === 'text' || kind === 'thinking') && typeof text === 'string')
                this.emit({ type: kind, text, partial, blockId: id, turnId, source: this.turnSource, backgroundId: this.turnBackgroundId });
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
