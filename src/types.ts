export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
export type Args = Record<string, unknown>;
export interface Resource { key: string; kind: 'image' | 'file'; name: string; }
export interface Message {
    id: string; chatId: string; chatType: 'group' | 'p2p'; senderId: string;
    senderName?: string; senderIsBot?: boolean; senderIsSelf?: boolean; text: string; time: number;
    mentioned: boolean; replyTo?: string; threadId?: string; resources: Resource[];
    /** Fixed compatibility explanation, never the raw event or provider error. */
    inputIssue?: string;
    /** Set by the adapter only for standalone image inputs, not rich text or captions. */
    imageOnly?: boolean;
}
export interface Room {
    key: string; chatId: string; projectId: string; dmUser?: string;
    maintainers: string[]; members?: string[];
}
export interface Session {
    id: string; roomKey: string; ccId?: string;
    state: 'open' | 'stopped' | 'archived'; createdAt: number;
}
export interface ImageBatch {
    id: string; deadline: number; closed: boolean; maxImages: number; maxBytes: number;
}
export interface Delivery {
    uuid: string; roomKey: string; sessionId: string; message: Message;
    background: Message[]; quote?: Message;
    command?: { name: 'compact'; instructions: string };
    /** An unsent supplement is valid only while this foreground input is still running. */
    supplementTo?: string;
    /** Persisted on each original row; the fixed window survives service restart. */
    imageBatch?: ImageBatch;
    /** Wire-only aggregation. Original rows, UUIDs and receipt ownership remain separate. */
    batchInputs?: Delivery[];
    attachments: { path: string; name: string }[];
}
export interface ActionSpec {
    id: string; description: string; executable: string; args: string[]; cwd?: string;
    env: Record<string, string>; vars: Record<string, string>; readOnly: boolean; timeoutMs: number;
    parameters: Record<string, {
        description?: string; pattern?: string; enum?: string[];
        maxLength: number; workspaceFile?: boolean;
    }>;
}
export interface Project {
    id: string; maintainers: string[]; seedDir?: string; trustedConfigDir?: string;
    permissionMode: 'default' | 'acceptEdits';
    allowedTools: string[];
    actions: ActionSpec[];
}
export interface Config {
    tenantId: string; appId: string; appSecretEnv: string; dataDir: string;
    runtime: { maxWorkers: number; idleMs: number };
    model: {
        baseUrl: string; tokenEnv: string; model: string; smallModel: string;
        extraHeaders: Record<string, string>; allowHttp: boolean;
    };
    projects: Project[];
    groups: { chatId: string; projectId: string; members?: string[] }[];
    dms: { userId: string; projectId: string }[];
    limits: {
        backgroundMessages: number; backgroundMs: number; textChars: number; inboxPerRoom: number;
        attachmentBytes: number; approvalMs: number; updateMs: number; messagesPerMinute: number;
        imageBatchMs: number; imageBatchMaxImages: number; imageBatchBytes: number;
    };
}
export type ReceiptState = 'processing' | 'done' | 'clear';
export type ReceiptApplied = ReceiptState | 'attempt:processing' | 'attempt:done';
export interface HistoryQuery {
    limit: number; startTime?: number; endTime?: number; pageToken?: string; threadId?: string;
}
export interface HistoryPage { messages: Message[]; nextPageToken?: string; }
export interface Origin { id: string; senderId: string; }
export interface Transport {
    send(room: Room, text: string, replyTo?: string, threadId?: string): Promise<string>;
    card(room: Room, card: object, replyTo?: string, threadId?: string): Promise<string>;
    update(messageId: string, card: object): Promise<void>;
    download(messageId: string, resource: Resource, signal?: AbortSignal): Promise<Uint8Array>;
    fetch(messageId: string, room?: Room): Promise<Message | undefined>;
    history(room: Room, query: HistoryQuery): Promise<HistoryPage>;
    receipt(messageId: string, state?: ReceiptState, previouslyApplied?: ReceiptApplied): Promise<void>;
    file(room: Room, path: string, name: string): Promise<string>;
}
export type WorkerEvent =
    { type: 'ready'; ccId: string } |
    { type: 'accepted'; uuid: string } |
    { type: 'consumed'; uuid: string } |
    { type: 'text' | 'thinking'; text: string; partial: boolean; blockId?: string; turnId?: string } |
    { type: 'compaction'; phase: 'start' | 'complete'; trigger: 'manual' | 'auto' } |
    { type: 'result'; text: string; success: boolean; uuid?: string; uuids?: string[]; usage?: Args;
        /** Native result identity, distinct from the originating input UUID. */
        resultId?: string; models?: string[];
        replyToUuid?: string; compact?: 'compacted' | 'not_needed' | 'failed' } |
    { type: 'activity'; tool?: string; background?: number; pinned?: boolean; state?: 'idle' | 'running' | 'requires_action' } |
    { type: 'request'; id: string; kind: 'permission' | 'question' | 'action' | 'send_file' | 'attachment' | 'history'; name: string; input: Args } |
    { type: 'error'; message: string } |
    { type: 'exit'; code: number | null; cleanupConfirmed?: boolean };
export interface Worker {
    send(delivery: Delivery): Promise<void>;
    respond(id: string, value: Args): void;
    stop(): Promise<void>;
}
export interface Workspace { root: string; work: string; home: string; inbound: string; }
export type WorkerFactory = (
    room: Room, session: Session, onEvent: (event: WorkerEvent) => void, workspace?: Workspace
) => Promise<Worker>;
