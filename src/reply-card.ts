/** One Card 2.0 message with independent public reasoning and answer regions. */
import type { Args } from './types.ts';
import type { NativeContext } from './native-context.ts';
export interface ReplyDetails { model?: string; projectPath?: string; context?: NativeContext; usage?: Args; }
interface ReplyCardMeta extends ReplyDetails { thinkingSteps?: readonly string[]; elapsedMs?: number; success?: boolean; }
function snippet(text: string): string {
    const chars = Array.from(text.trim());
    return chars.length > 180 ? chars.slice(0, 180).join('') + '...' : chars.join('');
}
function duration(milliseconds: number): string {
    const seconds = Math.max(0, Math.floor(milliseconds / 1000));
    return seconds < 60 ? `${(Math.max(0, milliseconds) / 1000).toFixed(1)} 秒` : seconds < 3600 ? `${Math.floor(seconds / 60)} 分 ${String(seconds % 60).padStart(2, '0')} 秒` : `${Math.floor(seconds / 3600)} 小时 ${String(Math.floor(seconds % 3600 / 60)).padStart(2, '0')} 分`;
}
function tokens(value: number): string { return value < 1000 ? String(value) : value < 1000000 ? `${(value / 1000).toFixed(1)}k` : `${(value / 1000000).toFixed(1)}M`; }
function count(value: unknown): value is number { return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0; }
function statusLine(meta: ReplyCardMeta): string {
    const parts = meta.model ? [meta.model.replace(/[\r\n]/g, ' ')] : [];
    const usage: string[] = [];
    for (const [key, label] of [['output_tokens', 'out'], ['input_tokens', 'in'], ['cache_creation_input_tokens', 'cw'], ['cache_read_input_tokens', 'cr']] as const) {
        const value = meta.usage?.[key]; if (count(value) && value > 0) usage.push(`${label} ${tokens(value)}`);
    }
    if (usage.length) parts.push(usage.join(' '));
    const input = meta.context?.inputTokens, window = meta.context?.windowTokens;
    parts.push(count(input) && count(window) && window > 0 ? `ctx ${Math.floor(input / window * 100)}%（末次输入 ${tokens(input)} / ${tokens(window)}）`
        : count(input) ? `ctx ${tokens(input)} / 未知（末次输入）` : 'ctx 未知');
    return parts.join(' · ');
}
export function replyCard(title: string, answer: string, thinking = '', final = false, meta: ReplyCardMeta = {}): object {
    const elements: object[] = [];
    const steps = (meta.thinkingSteps ?? [thinking]).filter(step => step.trim());
    if (steps.length || !final && meta.thinkingSteps?.length) elements.push({
        tag: 'collapsible_panel', expanded: !final, background_color: 'grey',
        border: { color: 'grey' }, vertical_spacing: '8px', padding: '4px 8px',
        header: { title: { tag: 'plain_text', content: steps.length ? `Reasoning (${steps.length})` : 'Reasoning' } },
        elements: [
            ...(steps.length > 10 ? [{ tag: 'div', text: { tag: 'plain_text', text_size: 'notation', text_color: 'grey', content: `... ${steps.length - 10} earlier steps hidden` } }] : []),
            ...(steps.length ? steps.slice(-10).map(step => ({ tag: 'div', text: { tag: 'plain_text', text_size: 'notation', text_color: 'grey', content: snippet(step) },
                icon: { tag: 'standard_icon', token: 'mindmap_outlined' } })) : [{ tag: 'div', text: { tag: 'plain_text', text_size: 'notation', text_color: 'grey', content: 'Thinking...' } }])
        ]
    });
    elements.push({ tag: 'markdown', element_id: 'reply_answer', content: answer.slice(0, 24000) || (final ? '本轮已结束。' : steps.length ? '\u200b' : '处理中…') });
    if (final && (meta.elapsedMs !== undefined || meta.model || meta.projectPath || meta.context || meta.usage)) {
        elements.push({ tag: 'hr' });
        if (meta.elapsedMs !== undefined) elements.push({ tag: 'markdown', text_size: 'notation', content: `⏱ 用时 ${duration(meta.elapsedMs)}` });
        elements.push({ tag: 'markdown', text_size: 'notation', content: statusLine(meta) });
        if (meta.projectPath) elements.push({ tag: 'markdown', text_size: 'notation', content: meta.projectPath.replace(/[\r\n]/g, ' ') });
    }
    const stopped = /停止|取消/.test(title), failed = meta.success === false || /失败|错误|中断/.test(title);
    const template = !final ? 'blue' : stopped ? 'grey' : failed ? 'red' : 'green';
    return { schema: '2.0', config: { width_mode: 'default', update_multi: true, streaming_mode: !final,
        summary: { content: final ? answer.slice(0, 80) || title : '正在生成回复…' },
        ...(final ? {} : { streaming_config: { print_frequency_ms: { default: 70 }, print_step: { default: 1 }, print_strategy: 'fast' } }) },
        header: { template, title: { tag: 'plain_text', content: title === '处理中' && steps.length ? '思考中' : title } }, body: { elements } };
}
