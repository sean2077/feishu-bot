package gateway

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/sean2077/feishu-bot/internal/core"
)

// supplementReceipt renders the visible receipt for an accepted supplement.
// A card-form supplement has no human message bubble, so its receipt carries
// the author and the full text. The text is fenced with a longer run of
// backticks than any run it contains, which keeps user Markdown and mentions
// inert while preserving the original content.
//
// /btw supplements already have a visible message; they get an
// attribution-only receipt to avoid duplicating the content.
func supplementReceipt(m core.Message) string {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, text)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	who := "补充者"
	if m.SenderID != "" {
		who = fmt.Sprintf(`<at user_id="%s">补充者</at>`, m.SenderID)
	}
	if m.InteractionMessageID == "" {
		return who + " 补充已交给原生输入流。"
	}
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fmt.Sprintf("%s 补充了任务：\n\n%s\n%s\n%s", who, fence, text, fence)
}
