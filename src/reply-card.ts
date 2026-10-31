/** One Card 2.0 message with independent public reasoning and answer regions. */
export function replyCard(title: string, answer: string, thinking = '', final = false): object {
    const elements: object[] = [];
    if (thinking) elements.push({
        tag: 'collapsible_panel', expanded: !final,
        header: { title: { tag: 'plain_text', content: '思考' } },
        elements: [{ tag: 'markdown', content: thinking.slice(-8000) }]
    });
    elements.push({ tag: 'markdown', content: answer.slice(0, 24000) || (final ? '本轮已结束。' : '处理中…') });
    return { schema: '2.0', config: { width_mode: 'fill', update_multi: true }, header: { title: { tag: 'plain_text', content: title } }, body: { elements } };
}
