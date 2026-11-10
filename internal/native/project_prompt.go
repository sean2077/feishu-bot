package native

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

const maxProjectPromptBytes = 16 << 10

// loadProjectPrompt reads the configured project prompt snapshot. Prompt
// failures are fail-open: the worker starts without it and the gateway audits
// a fixed reason. The file is read once per native process.
func loadProjectPrompt(p core.Project, w core.Workspace) (text, reason string) {
	if p.AppendSystemPrompt != "" && p.AppendSystemPromptFile != "" {
		return "", "configuration"
	}
	if p.AppendSystemPrompt != "" {
		switch {
		case len(p.AppendSystemPrompt) > maxProjectPromptBytes:
			return "", "too_large"
		case !utf8.ValidString(p.AppendSystemPrompt):
			return "", "invalid_utf8"
		case strings.ContainsRune(p.AppendSystemPrompt, 0):
			return "", "nul"
		}
		return p.AppendSystemPrompt, ""
	}
	if p.AppendSystemPromptFile == "" {
		return "", ""
	}
	if (w.Home != "" && core.Inside(w.Home, p.AppendSystemPromptFile)) || (w.Root != "" && core.Inside(w.Root, p.AppendSystemPromptFile)) {
		return "", "path"
	}
	file, err := os.Open(p.AppendSystemPromptFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "missing"
		}
		return "", "unreadable"
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxProjectPromptBytes+1))
	if err != nil {
		return "", "unreadable"
	}
	switch {
	case len(data) > maxProjectPromptBytes:
		return "", "too_large"
	case !utf8.Valid(data):
		return "", "invalid_utf8"
	case strings.ContainsRune(string(data), 0):
		return "", "nul"
	case len(data) == 0:
		return "", ""
	}
	return string(data), ""
}

func projectPromptText(prompt string) string {
	if prompt == "" {
		return ""
	}
	return "Project instructions:\n" + prompt
}
