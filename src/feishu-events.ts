import type { LarkChannel } from '@larksuite/channel';
import type { Gateway } from './gateway.ts';
import { normalizeFeishuMessage } from './feishu-input.ts';
import { feishuDiagnostic } from './feishu-diagnostics.ts';

/** The production event entry, also exercised with a real SDK and local fixtures. */
export function bindFeishuInbound(channel: LarkChannel, gateway: Gateway,
    log: (event: string, detail: object) => void, closed: () => boolean): void {
    channel.on('message', async message => {
        if (closed()) return;
        try { await gateway.message(normalizeFeishuMessage(message)); }
        catch (error) { log('inbound_failed', feishuDiagnostic(error)); }
    });
    channel.on('cardAction', async event => {
        if (closed() || !event.messageId) return {};
        const content = await gateway.approvals.click(event.chatId, event.operator.openId, event.action.value, event.messageId);
        return content ? { toast: { type: 'info' as const, content } } : {};
    });
}
