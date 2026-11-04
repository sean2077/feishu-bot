import type { Args } from './types.ts';

/** Complete only inputs the host actually sent, using a retained native human
 * anchor solely when an ordinary foreground result lacks usable raw stamps. */
export function nativeResultOrigin(event: Args, knownInputs: ReadonlySet<string>, replyToUuid?: string,
    source?: 'foreground' | 'background' | 'unattributed'): { uuid?: string; uuids: string[] } {
    const consumed = Array.isArray(event.user_message_uuids) ? event.user_message_uuids : [event.user_message_uuid];
    let uuid = typeof event.user_message_uuid === 'string' && knownInputs.has(event.user_message_uuid) ? event.user_message_uuid : undefined;
    const uuids = consumed.filter((id): id is string => typeof id === 'string' && knownInputs.has(id));
    const origin = event.origin as Args | undefined;
    const foreground = source !== 'background' && !event.parent_tool_use_id && (!origin || (origin.kind === 'human' && !origin.senderTaskId));
    if (!uuid && !uuids.length && foreground && replyToUuid && knownInputs.has(replyToUuid)) {
        uuid = replyToUuid;
        uuids.push(replyToUuid);
    }
    return { uuid, uuids };
}
