import type { LarkChannel } from '@larksuite/channel';
import type { Gateway } from './gateway.ts';
import { normalizeFeishuMessage } from './feishu-input.ts';
import { feishuDiagnostic } from './feishu-diagnostics.ts';
import { hash } from './util.ts';

/** The production event entry, also exercised with a real SDK and local fixtures. */
export function bindFeishuInbound(channel: LarkChannel, gateway: Gateway,
    log: (event: string, detail: object) => void, closed: () => boolean): void {
    const diagnostic = (event: string, detail: object) => { try { log(event, detail); } catch { } };
    channel.on('reject', event => {
        if (closed()) return;
        const reason = ['group_not_allowed', 'sender_not_allowed', 'dm_disabled', 'no_mention', 'mention_all_blocked', 'bot_loop'].includes(event.reason)
            ? event.reason : 'other';
        // Public policy result metadata only; rejected bodies never enter Gateway.
        diagnostic('feishu_policy_rejected', { reason, message: hash(event.messageId).slice(0, 16),
            chat: hash(event.chatId).slice(0, 16), sender: hash(event.senderId).slice(0, 16) });
    });
    channel.on('message', async message => {
        if (closed()) return;
        try { await gateway.message(normalizeFeishuMessage(message)); }
        catch (error) { log('inbound_failed', feishuDiagnostic(error)); }
    });
    channel.on('cardAction', async event => {
        if (closed() || !event.messageId) return {};
        const started = Date.now();
        const status = channel.getConnectionStatus();
        const value = event.action.value;
        diagnostic('card_callback_started', {
            valueType: typeof value === 'string' ? 'string' : value && typeof value === 'object' && !Array.isArray(value) ? 'object' : 'other',
            hasOperator: !!event.operator.openId, hasChat: !!event.chatId,
            ...(status && ['idle', 'connecting', 'connected', 'reconnecting', 'failed'].includes(status.state)
                ? { connectionState: status.state, reconnectAttempts: status.reconnectAttempts } : {})
        });
        try {
            const content = await gateway.approvals.click(event.chatId, event.operator.openId, event.action.value, event.messageId);
            const response = !content ? 'silent' : content.includes('卡片刷新失败') ? 'failure'
                : content.startsWith('已记录你的选择') ? 'selected' : content === '已提交' || content === '已批准一次' ? 'submitted'
                : content === '请回答所有问题' ? 'incomplete' : content === '已拒绝' || content.startsWith('请求已结束') ? 'ended' : 'failure';
            diagnostic('card_callback_completed', { elapsedMs: Math.max(0, Date.now() - started), response });
            return content ? { toast: { type: 'info' as const, content } } : {};
        } catch (error) {
            diagnostic('card_callback_failed', { elapsedMs: Math.max(0, Date.now() - started), ...feishuDiagnostic(error) });
            throw error;
        }
    });
}
