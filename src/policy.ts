import type { Config, Message, Room } from './types.ts';

/** Chat identity is stable across threads, process recycling and ordinary restarts. */
export function roomFor(config: Config, chatId: string, dmUser?: string): Room | undefined {
    const binding = dmUser === undefined
        ? config.groups.find(g => g.chatId === chatId)
        : config.dms.find(d => d.userId === dmUser);
    if (!binding) return undefined;
    const project = config.projects.find(p => p.id === binding.projectId);
    if (!project) return undefined;
    // Missing members means maintainers only, never everybody in the group.
    const members = 'members' in binding ? binding.members ?? [] : [];
    return {
        key: JSON.stringify([config.tenantId, config.appId, project.id, dmUser ? 'p2p' : 'group', chatId, dmUser ?? null]),
        chatId, projectId: project.id, maintainers: project.maintainers, members, dmUser
    };
}
export function roomFromKey(config: Config, key: string): Room | undefined {
    try {
        const parts: unknown = JSON.parse(key);
        if (!Array.isArray(parts) || parts.length !== 6 || typeof parts[4] !== 'string') return undefined;
        if (parts[3] !== 'group' && parts[3] !== 'p2p') return undefined;
        if (parts[3] === 'p2p' && typeof parts[5] !== 'string') return undefined;
        const room = roomFor(config, parts[4], parts[3] === 'p2p' ? parts[5] as string : undefined);
        return room?.key === key ? room : undefined;
    } catch { return undefined; }
}
export function authorizedAuthor(room: Room, senderId: string): boolean {
    if (!senderId) return false;
    return room.dmUser !== undefined ? room.dmUser === senderId
        : room.maintainers.includes(senderId) || (room.members ?? []).includes(senderId);
}
export function route(config: Config, message: Message): Room | undefined {
    if (message.senderIsBot || !message.senderId || !message.chatId || !message.id) return undefined;
    if (message.chatType !== 'p2p' && message.chatType !== 'group') return undefined;
    const room = roomFor(config, message.chatId, message.chatType === 'p2p' ? message.senderId : undefined);
    return room && authorizedAuthor(room, message.senderId) ? room : undefined;
}
export function triggers(message: Message): boolean {
    return message.chatType === 'p2p' || message.mentioned === true;
}
/** Used for explicit history/quote reads, not for deciding whether to trigger. */
export function readable(room: Room, message: Message): boolean {
    if (!message.id || message.chatId !== room.chatId) return false;
    if (message.chatType !== (room.dmUser ? 'p2p' : 'group')) return false;
    return message.senderIsBot ? message.senderIsSelf === true : authorizedAuthor(room, message.senderId);
}
