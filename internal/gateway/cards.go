package gateway

import (
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
	"time"
	"unicode/utf8"
)

func DisplayCard(title, text string, buttons ...any) core.Args {
	elements := []any{core.Args{"tag": "markdown", "content": text}}
	elements = append(elements, buttons...)
	return core.Args{"schema": "2.0", "config": core.Args{"wide_screen_mode": true}, "header": core.Args{"title": core.Args{"tag": "plain_text", "content": title}}, "body": core.Args{"elements": elements}}
}
func button(label string, value core.Args, style string) core.Args {
	if style == "" {
		style = "default"
	}
	// Card JSON 2.0 declares click callbacks on the component's behaviors. A
	// top-level value is the card 1.0 form and does not declare a callback here,
	// so the click can appear to do nothing.
	return core.Args{"tag": "button", "text": core.Args{"tag": "plain_text", "content": cut(label, 80)}, "type": style, "behaviors": []any{core.Args{"type": "callback", "value": value}}}
}

// actionRow places buttons side by side and makes each fill its column.
func actionRow(buttons ...core.Args) core.Args {
	columns := make([]any, 0, len(buttons))
	for _, b := range buttons {
		b["width"] = "fill"
		columns = append(columns, core.Args{"tag": "column", "width": "weighted", "weight": 1, "elements": []any{b}})
	}
	return core.Args{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "medium", "columns": columns}
}

// optionRow keeps a long label and its description in the text column and a
// compact action on the right, instead of stacking a full-width button under a
// duplicate label line.
func optionRow(label, description string, value core.Args, style string) core.Args {
	text := []any{core.Args{"tag": "markdown", "content": "**" + label + "**"}}
	if description != "" {
		text = append(text, core.Args{"tag": "markdown", "text_size": "notation", "content": description})
	}
	return core.Args{
		"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "medium",
		"columns": []any{
			core.Args{"tag": "column", "width": "weighted", "weight": 1, "vertical_spacing": "2px", "elements": text},
			core.Args{"tag": "column", "width": "auto", "horizontal_align": "right", "elements": []any{button("选择", value, style)}},
		},
	}
}

type replyDetails struct {
	Model, ProjectPath string
	Context, Usage     core.Args
}

func tokenLabel(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}
func statusLine(d replyDetails) string {
	var s []string
	if d.Model != "" {
		s = append(s, strings.NewReplacer("\r", " ", "\n", " ").Replace(d.Model))
	}
	for _, f := range []struct{ k, label string }{{"output_tokens", "out"}, {"input_tokens", "in"}, {"cache_creation_input_tokens", "cw"}, {"cache_read_input_tokens", "cr"}} {
		if n, ok := numberArg(d.Usage, f.k); ok && n > 0 {
			s = append(s, f.label+" "+tokenLabel(n))
		}
	}
	input, ok := numberArg(d.Context, "inputTokens")
	window, wok := numberArg(d.Context, "windowTokens")
	if ok && wok && window > 0 {
		s = append(s, fmt.Sprintf("ctx %d%%（末次输入 %s / %s）", input*100/window, tokenLabel(input), tokenLabel(window)))
	} else if ok {
		s = append(s, "ctx "+tokenLabel(input)+" / 未知（末次输入）")
	} else {
		s = append(s, "ctx 未知")
	}
	return strings.Join(s, " · ")
}
func replyCard(title, answer string, thinking []string, total int, final bool, d replyDetails, elapsed time.Duration, success bool) core.Args {
	card, _ := boundedReplyCard(title, answer, thinking, total, final, d, elapsed, success, nil)
	return card
}

// The answer is the only large variable field. Bound metadata separately so
// even a hostile model label or project path cannot consume the answer budget.
func buildReplyCard(title, answer string, thinking []string, total int, final bool, d replyDetails, elapsed time.Duration, success bool) core.Args {
	title = cut(title, 120)
	d.Model = cut(d.Model, 256)
	d.ProjectPath = cut(d.ProjectPath, 512)
	elements := []any{}
	if total > 0 {
		steps := []any{}
		if total > len(thinking) {
			steps = append(steps, core.Args{"tag": "div", "text": core.Args{"tag": "plain_text", "text_size": "notation", "text_color": "grey", "content": fmt.Sprintf("... %d earlier steps hidden", total-len(thinking))}})
		}
		for _, t := range thinking {
			snippet := cut(strings.TrimSpace(t), 180)
			if len([]rune(strings.TrimSpace(t))) > 180 {
				snippet += "..."
			}
			steps = append(steps, core.Args{"tag": "div", "text": core.Args{"tag": "plain_text", "text_size": "notation", "text_color": "grey", "content": snippet}, "icon": core.Args{"tag": "standard_icon", "token": "mindmap_outlined"}})
		}
		elements = append(elements, core.Args{"tag": "collapsible_panel", "expanded": !final, "background_color": "grey", "border": core.Args{"color": "grey"}, "vertical_spacing": "8px", "padding": "4px 8px", "header": core.Args{"title": core.Args{"tag": "plain_text", "content": fmt.Sprintf("Reasoning (%d)", total)}}, "elements": steps})
	}
	body := answer
	if body == "" {
		if final {
			body = "本轮已结束。"
		} else if total > 0 {
			body = "\u200b"
		} else {
			body = "处理中…"
		}
	}
	elements = append(elements, core.Args{"tag": "markdown", "element_id": "reply_answer", "content": body})
	if final {
		elements = append(elements, core.Args{"tag": "hr"}, core.Args{"tag": "markdown", "text_size": "notation", "content": fmt.Sprintf("⏱ 用时 %.1f 秒", elapsed.Seconds())}, core.Args{"tag": "markdown", "text_size": "notation", "content": statusLine(d)})
		if d.ProjectPath != "" {
			elements = append(elements, core.Args{"tag": "markdown", "text_size": "notation", "content": strings.NewReplacer("\r", " ", "\n", " ").Replace(d.ProjectPath)})
		}
	}
	color := "blue"
	if final {
		color = "green"
		if strings.Contains(title, "停止") || strings.Contains(title, "取消") {
			color = "grey"
		} else if !success || strings.Contains(title, "失败") || strings.Contains(title, "中断") {
			color = "red"
		}
	}
	if title == "处理中" && total > 0 {
		title = "思考中"
	}
	summary := "正在生成回复…"
	if final {
		summary = cut(answer, 80)
		if summary == "" {
			summary = title
		}
	}
	cfg := core.Args{"width_mode": "default", "update_multi": true, "streaming_mode": !final, "summary": core.Args{"content": summary}}
	if !final {
		cfg["streaming_config"] = core.Args{"print_frequency_ms": core.Args{"default": 70}, "print_step": core.Args{"default": 1}, "print_strategy": "fast"}
	}
	return core.Args{"schema": "2.0", "config": cfg, "header": core.Args{"template": color, "title": core.Args{"tag": "plain_text", "content": title}}, "body": core.Args{"elements": elements}}
}

// bytePrefix never splits a UTF-8 code point and avoids allocating []rune for
// every preview. JSON is still measured after escaping, not estimated by bytes.
func bytePrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func boundedReplyCard(title, answer string, thinking []string, total int, final bool, d replyDetails, elapsed time.Duration, success bool, actions []any) (core.Args, bool) {
	makeCard := func(body string) core.Args {
		card := buildReplyCard(title, body, thinking, total, final, d, elapsed, success)
		if len(actions) > 0 {
			body := argsOf(card["body"])
			elements, _ := body["elements"].([]any)
			body["elements"] = append(elements, actions...)
			card["body"] = body
		}
		return card
	}
	preview := bytePrefix(answer, core.CardPayloadLimit)
	card := makeCard(preview)
	n, err := core.CardPayloadBytes(card)
	if len(preview) == len(answer) && err == nil && n <= core.CardPayloadLimit {
		return card, false
	}
	suffix := "\n\n（预览已截断，完成后附完整答案文件。）"
	if final {
		suffix = "\n\n（完整答案另附文件。）"
	}
	low, high := 0, len(preview)
	for low < high {
		mid := low + (high-low+1)/2
		candidate := makeCard(bytePrefix(preview, mid) + suffix)
		size, e := core.CardPayloadBytes(candidate)
		if e == nil && size <= core.CardPayloadLimit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return makeCard(bytePrefix(preview, low) + suffix), true
}
