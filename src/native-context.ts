import type { Args } from './types.ts';

export interface NativeContext { inputTokens?: number; windowTokens?: number; }
const nativeModelName = (name: string): string => name.replace(/\[1m\]$/i, '');
function count(value: unknown): number | undefined {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : undefined;
}
function inputSize(raw: unknown): number | undefined {
    if (!raw || typeof raw !== 'object') return undefined;
    const usage = raw as Args;
    const input = count(usage.input_tokens);
    // Anthropic cache counters are optional. Omission means no reported cache
    // contribution; explicit null/invalid counters cannot produce a total.
    const read = usage.cache_read_input_tokens === undefined ? 0 : count(usage.cache_read_input_tokens);
    const write = usage.cache_creation_input_tokens === undefined ? 0 : count(usage.cache_creation_input_tokens);
    if (input === undefined || read === undefined || write === undefined) return undefined;
    const total = input + read + write;
    return Number.isSafeInteger(total) ? total : undefined;
}

/** Last observed main-loop request input, not aggregate turn usage or the
 * exact post-answer context. No transcript scan or extra model call. */
export class NativeContextSnapshot {
    private model?: string;
    get modelName(): string | undefined { return this.model; }
    private messageId?: string;
    private inputTokens?: number;
    private streamSnapshot = false;
    observe(event: Args): void {
        if (event.parent_tool_use_id) return;
        if (event.type === 'system' && event.subtype === 'compact_boundary') { this.reset(); return; }
        const stream = event.type === 'stream_event' ? event.event as Args | undefined : undefined;
        const start = stream?.type === 'message_start';
        const message = start ? stream.message as Args | undefined : event.type === 'assistant' ? event.message as Args | undefined : undefined;
        if (!message) return;
        const id = typeof message.id === 'string' ? message.id : undefined;
        if (start || id !== this.messageId) {
            this.model = typeof message.model === 'string' ? message.model : undefined;
            this.messageId = id;
            this.inputTokens = inputSize(message.usage);
            this.streamSnapshot = start && this.inputTokens !== undefined;
        } else if (!this.streamSnapshot) {
            this.model = typeof message.model === 'string' ? message.model : this.model;
            const size = inputSize(message.usage);
            if (size !== undefined) this.inputTokens = size;
        }
    }
    result(event: Args, requestedModel?: string): NativeContext | undefined {
        const rows = event.modelUsage;
        let row: unknown;
        if (this.model && rows && typeof rows === 'object' && !Array.isArray(rows)) {
            const usage = rows as Args;
            // A native context selector decorates the request name, not the API
            // response model. It identifies a metadata row; it never supplies
            // a guessed numerical window size.
            const requestKey = (name: string) => name.replace(/\[1m\]$/i, '[1m]');
            const selected = requestedModel && nativeModelName(requestedModel) === this.model
                ? Object.keys(usage).filter(name => requestKey(name) === requestKey(requestedModel)) : [];
            if (selected.length === 1) row = usage[selected[0]!];
            else if (!selected.length) {
                if (Object.hasOwn(usage, this.model)) row = usage[this.model];
                else {
                    const matches = Object.keys(usage).filter(name => nativeModelName(name) === this.model);
                    if (matches.length === 1) row = usage[matches[0]!];
                }
            }
        }
        const rawWindow = row && typeof row === 'object' ? count((row as Args).contextWindow) : undefined;
        const windowTokens = rawWindow && rawWindow > 0 ? rawWindow : undefined;
        return this.inputTokens === undefined && windowTokens === undefined ? undefined : {
            ...(this.inputTokens === undefined ? {} : { inputTokens: this.inputTokens }),
            ...(windowTokens === undefined ? {} : { windowTokens })
        };
    }
    reset(): void { this.model = undefined; this.messageId = undefined; this.inputTokens = undefined; this.streamSnapshot = false; }
}
