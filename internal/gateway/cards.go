package gateway

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

// workspaceLabel controls the working-directory line on a reply card. Group
// chats default to the project id so absolute deployment paths do not leak;
// DMs keep the full path, matching the previous behavior. An explicit project
// setting overrides both defaults. Existing saved cards are left untouched.
func (g *Gateway) workspaceLabel(room core.Room, projectID, work string) string {
	mode := ""
	if project, ok := g.config.Project(projectID); ok {
		mode = project.DisplayWorkspace
	}
	if mode == "" {
		if room.DMUser != "" {
			mode = "full"
		} else {
			mode = "project"
		}
	}
	switch mode {
	case "full":
		return work
	case "basename":
		return filepath.Base(filepath.Clean(work))
	case "project":
		if projectID != "" {
			return projectID
		}
		return work
	default:
		return ""
	}
}

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

// replyDoneText is the status shown on the final card of a successfully
// delivered reply. Feedback eligibility checks the rendered row against this
// shared value instead of duplicating the display wording.
const replyDoneText = "已回复"

func replyStatusContent(text string) string { return "● " + text }

// replyStatus turns the internal turn title into the compact status label shown
// at the top of the card. The color carries the same state the header template
// used to carry, without a full-width colored band.
func replyStatus(title string, total int, final, success bool) (string, string) {
	text := cut(title, 40)
	color := "blue"
	switch {
	case final && (strings.Contains(title, "停止") || strings.Contains(title, "取消")):
		color = "grey"
	case final && (!success || strings.Contains(title, "失败") || strings.Contains(title, "中断")):
		color = "red"
	case final:
		color = "green"
	case strings.Contains(title, "等待") || strings.Contains(title, "审批") || strings.Contains(title, "回答"):
		color = "orange"
	}
	if !final && title == "处理中" && total > 0 {
		text = "思考中"
	}
	if final && success && title == "回复" {
		text = replyDoneText
	}
	return text, color
}

// appendReplyNotice adds or replaces the single visible auto-mode safety
// notice. The element_id stays within CardKit's 20-character limit.
func appendReplyNotice(card core.Args, notice string) core.Args {
	body := argsOf(card["body"])
	if body == nil || notice == "" {
		return card
	}
	elements, _ := body["elements"].([]any)
	kept := elements[:0]
	for _, item := range elements {
		if stringArg(argsOf(item), "element_id") != "reply_auto_notice" {
			kept = append(kept, item)
		}
	}
	body["elements"] = append(kept, core.Args{"tag": "markdown", "element_id": "reply_auto_notice", "text_size": "notation", "content": notice})
	card["body"] = body
	return card
}

const defaultCardTitle = "飞书助手"

// cardTitle returns the configured bot name with the legacy default fallback.
// The value is configuration, not model output, but it still sits inside a
// markdown title and must be escaped by replyStatusRow.
func (g *Gateway) cardTitle() string {
	if g.config.CardTitle == "" {
		return defaultCardTitle
	}
	return g.config.CardTitle
}

func escapeCardTitle(title string) string {
	var b strings.Builder
	for _, r := range title {
		if strings.ContainsRune("\\*_[]`<>~|#!&", r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// replyStatusIcon maps gateway-owned status text to a built-in Feishu icon.
// The icons are part of the standard icon library, so no image upload or
// client-side animation is involved.
func replyStatusIcon(text, color string, final, success bool) string {
	switch {
	case text == "思考中" || text == "处理中":
		return "loading_outlined"
	case final && success && color == "green":
		return "succeed_filled"
	case final && color == "red":
		return "error_filled"
	case final && color == "grey":
		return "stop_outlined"
	case !final && color == "orange":
		return "alarm-clock_outlined"
	}
	return ""
}

func replyStatusRow(cardTitle, text, color, iconToken string) core.Args {
	status := core.Args{"tag": "div", "text": core.Args{"tag": "plain_text", "text_size": "notation", "text_color": color, "content": replyStatusContent(text)}}
	if iconToken != "" {
		status["icon"] = core.Args{"tag": "standard_icon", "token": iconToken, "color": color}
	}
	return core.Args{
		"tag": "column_set", "element_id": "reply_status", "flex_mode": "none", "horizontal_spacing": "medium",
		"columns": []any{
			core.Args{"tag": "column", "width": "weighted", "weight": 1, "vertical_align": "center", "elements": []any{
				core.Args{"tag": "markdown", "content": "**" + escapeCardTitle(cardTitle) + "**"},
			}},
			core.Args{"tag": "column", "width": "auto", "horizontal_align": "right", "vertical_align": "center", "elements": []any{status}},
		},
	}
}

// setReplyCardStatus updates the visible state on a private card copy. Older
// saved cards may have a header, an unmarked compact row, or both; leaving either
// old status visible could contradict a pending delivery or cancellation.
func setReplyCardStatus(card core.Args, cardTitle, text, color string) core.Args {
	body := argsOf(card["body"])
	if body == nil {
		body = core.Args{}
	}
	elements, _ := body["elements"].([]any)
	row := replyStatusRow(cardTitle, cut(text, 40), color, "")
	replaced := false
	for i, value := range elements {
		element := argsOf(value)
		if stringArg(element, "element_id") == "reply_status" || (i == 0 && legacyReplyStatusRow(element)) {
			elements[i] = row
			replaced = true
			break
		}
	}
	if !replaced {
		elements = append([]any{row}, elements...)
	}
	body["elements"] = elements
	card["body"] = body
	delete(card, "header")
	return card
}

// The first compact-card release had no element_id. Recognize its exact wire
// shape, including the assistant label and layout, rather than replacing an
// arbitrary custom column_set or a row that merely contains status-like text.
func legacyReplyStatusRow(row core.Args) bool {
	columns, _ := row["columns"].([]any)
	if len(columns) != 2 {
		return false
	}
	right, _ := argsOf(columns[1])["elements"].([]any)
	if len(right) != 1 {
		return false
	}
	text := argsOf(argsOf(right[0])["text"])
	label := stringArg(text, "content")
	if !strings.HasPrefix(label, "● ") {
		return false
	}
	expected := replyStatusRow(defaultCardTitle, strings.TrimPrefix(label, "● "), stringArg(text, "text_color"), "")
	delete(expected, "element_id")
	return jsonText(row) == jsonText(expected)
}

// The answer is the only large variable field. Bound metadata separately so
// even a hostile model label or project path cannot consume the answer budget.
func buildReplyCard(cardTitle, title, answer string, thinking []string, total int, final bool, d replyDetails, elapsed time.Duration, success bool) core.Args {
	title = cut(title, 120)
	d.Model = cut(d.Model, 256)
	d.ProjectPath = cut(d.ProjectPath, 512)
	status, statusColor := replyStatus(title, total, final, success)
	elements := []any{replyStatusRow(cardTitle, status, statusColor, replyStatusIcon(status, statusColor, final, success))}
	var reasoning core.Args
	if total > 0 {
		steps := []any{}
		if total > len(thinking) {
			steps = append(steps, core.Args{"tag": "div", "text": core.Args{"tag": "plain_text", "text_size": "notation", "text_color": "grey", "content": fmt.Sprintf("另有 %d 步未显示", total-len(thinking))}})
		}
		for _, t := range thinking {
			snippet := cut(strings.TrimSpace(t), 180)
			if len([]rune(strings.TrimSpace(t))) > 180 {
				snippet += "..."
			}
			steps = append(steps, core.Args{"tag": "div", "text": core.Args{"tag": "plain_text", "text_size": "notation", "text_color": "grey", "content": snippet}, "icon": core.Args{"tag": "standard_icon", "token": "mindmap_outlined"}})
		}
		reasoning = core.Args{"tag": "collapsible_panel", "expanded": false, "vertical_spacing": "4px", "padding": "4px 8px", "header": core.Args{"title": core.Args{"tag": "plain_text", "content": fmt.Sprintf("思考过程 · %d 步", total)}}, "elements": steps}
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
	if reasoning != nil {
		elements = append(elements, reasoning)
	}
	if final {
		meta := fmt.Sprintf("⏱ %.1f 秒", elapsed.Seconds())
		if line := statusLine(d); line != "" {
			meta += " · " + line
		}
		elements = append(elements, core.Args{"tag": "markdown", "text_size": "notation", "content": meta})
		if d.ProjectPath != "" {
			elements = append(elements, core.Args{"tag": "markdown", "text_size": "notation", "content": strings.NewReplacer("\r", " ", "\n", " ").Replace(d.ProjectPath)})
		}
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
	return core.Args{"schema": "2.0", "config": cfg, "body": core.Args{"elements": elements}}
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
	return boundedReplyCardTitle(defaultCardTitle, title, answer, thinking, total, final, d, elapsed, success, actions)
}

func boundedReplyCardTitle(cardTitle, title, answer string, thinking []string, total int, final bool, d replyDetails, elapsed time.Duration, success bool, actions []any) (core.Args, bool) {
	makeCard := func(body string) core.Args {
		card := buildReplyCard(cardTitle, title, body, thinking, total, final, d, elapsed, success)
		if len(actions) > 0 {
			body := argsOf(card["body"])
			elements, _ := body["elements"].([]any)
			body["elements"] = append(elements, actions...)
			card["body"] = body
		}
		return card
	}
	preview := bytePrefix(answer, core.CardPayloadLimit)
	shown, needsFile := prepareReplyMarkdown(preview)
	card := makeCard(shown)
	n, err := core.CardPayloadBytes(card)
	if len(preview) == len(answer) && !needsFile && err == nil && n <= core.CardPayloadLimit {
		return card, false
	}
	suffix := "\n\n（预览已截断，完成后附完整答案文件。）"
	if final {
		suffix = "\n\n（完整答案另附文件。）"
	}
	if len(preview) == len(answer) && needsFile {
		notice := suffix
		if !final {
			notice = "\n\n（后续内容按原文显示，完成后附完整答案文件。）"
		}
		card = makeCard(shown + notice)
		if size, e := core.CardPayloadBytes(card); e == nil && size <= core.CardPayloadLimit {
			return card, true
		}
	}
	makePreview := func(n int) core.Args {
		body, _ := prepareReplyMarkdown(bytePrefix(preview, n))
		// Append the notice after closing a partial code fence, never inside it.
		return makeCard(body + suffix)
	}
	low, high := 0, len(preview)
	for low < high {
		mid := low + (high-low+1)/2
		candidate := makePreview(mid)
		size, e := core.CardPayloadBytes(candidate)
		if e == nil && size <= core.CardPayloadLimit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return makePreview(low), true
}
