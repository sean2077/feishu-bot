package gateway

import "strings"

// Feishu limits one Markdown component to four tables (not the separate card
// table-component limit). Keep a stable reply_answer element for streaming.
// https://open.feishu.cn/document/feishu-cards/feishu-card-cardkit/components/markdown
const replyMarkdownTableLimit = 4

// prepareReplyMarkdown changes only a display snapshot. The caller retains the
// original answer and requires its file when tables overflow. A streaming code
// block is temporarily closed without changing the accumulated source. This is
// a small block scan, not a Markdown rewriter: ordinary text stays byte-for-byte
// intact, including escaped pipes and complete code examples.
func prepareReplyMarkdown(text string) (content string, needsFile bool) {
	lines := strings.SplitAfter(text, "\n")
	fence, fenceQuote := "", ""
	tableQuote, inTable := "", false
	tables, offset := 0, 0
	for i, line := range lines {
		if line == "" && i == len(lines)-1 {
			break // SplitAfter's empty tail is not a new block-quote boundary.
		}
		body, quote, indented := markdownBlockLine(line)
		if inTable {
			_, pipes := markdownTableCells(body)
			if !indented && quote == tableQuote && pipes && markdownFenceOpens(body) == "" {
				offset += len(line)
				continue
			}
			inTable = false
		}
		if fence != "" && fenceQuote != "" && !strings.HasPrefix(quote, fenceQuote) {
			// A fenced block inside a quote ends with its container, even if
			// the author did not write a closing fence.
			fence = ""
		}
		if fence != "" {
			if !indented && quote == fenceQuote && markdownFenceCloses(body, fence) {
				fence = ""
			}
		} else if !indented {
			if opening := markdownFenceOpens(body); opening != "" {
				fence, fenceQuote = opening, quote
			} else if i+1 < len(lines) {
				next, nextQuote, nextIndented := markdownBlockLine(lines[i+1])
				if !nextIndented && quote == nextQuote && markdownTableStarts(body, next) {
					tables++
					if tables > replyMarkdownTableLimit {
						// Fence the remaining source as one literal block. This
						// preserves every later row and paragraph without guessing
						// where a partially streamed table ends.
						return text[:offset] + literalMarkdown(text[offset:]), true
					}
					tableQuote, inTable = quote, true
				}
			}
		}
		offset += len(line)
	}
	if fence != "" {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return text + fenceQuote + fence, false
	}
	return text, false
}

// Strip only block-quote markers and the indentation permitted before a fence.
// Four-space/tab-indented code must not consume the table budget.
func markdownBlockLine(line string) (body, quote string, indented bool) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	for {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 || strings.HasPrefix(trimmed, "\t") {
			return trimmed, quote, true
		}
		line = trimmed
		if !strings.HasPrefix(line, ">") {
			return line, quote, false
		}
		quote += "> "
		line = strings.TrimPrefix(line[1:], " ")
	}
}

func markdownFenceOpens(line string) string {
	if len(line) < 3 || line[0] != '`' && line[0] != '~' {
		return ""
	}
	n := 1
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 || line[0] == '`' && strings.Contains(line[n:], "`") {
		return ""
	}
	return line[:n]
}

func markdownFenceCloses(line, fence string) bool {
	n := 0
	for n < len(line) && line[n] == fence[0] {
		n++
	}
	return n >= len(fence) && strings.TrimSpace(line[n:]) == ""
}

func markdownTableStarts(header, delimiter string) bool {
	head, headPipes := markdownTableCells(header)
	dividers, dividerPipes := markdownTableCells(delimiter)
	if !headPipes || !dividerPipes || len(head) != len(dividers) {
		return false
	}
	for _, cell := range dividers {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimSuffix(strings.TrimPrefix(cell, ":"), ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func markdownTableCells(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	cells := []string{}
	start, escaped := 0, false
	for i := 0; i < len(line); i++ {
		if escaped {
			escaped = false
			continue
		}
		if line[i] == '\\' {
			escaped = true
		} else if line[i] == '|' {
			cells = append(cells, line[start:i])
			start = i + 1
		}
	}
	if len(cells) == 0 {
		return nil, false
	}
	cells = append(cells, line[start:])
	if strings.HasPrefix(line, "|") {
		cells = cells[1:]
	}
	if start == len(line) {
		cells = cells[:len(cells)-1]
	}
	return cells, len(cells) > 0
}

func literalMarkdown(text string) string {
	// A fence longer than any run in the source cannot be closed by an
	// embedded code sample. Choose the shorter of the two valid markers so a
	// model-generated long run does not needlessly consume the card budget.
	length, marker := 3, byte('`')
	maxRun := func(char byte) int {
		longest, run := 0, 0
		for i := 0; i < len(text); i++ {
			if text[i] == char {
				run++
				longest = max(longest, run)
			} else {
				run = 0
			}
		}
		return max(3, longest+1)
	}
	length = maxRun(marker)
	if other := maxRun('~'); other < length {
		length, marker = other, '~'
	}
	fence := strings.Repeat(string(marker), length)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return "\n" + fence + "\n" + text + fence
}
