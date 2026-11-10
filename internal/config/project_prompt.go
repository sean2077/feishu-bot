package config

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

const maxProjectPromptBytes = 16 << 10

// LoadProjectPrompt reads and validates the configured project prompt. It
// returns a fixed reason token (not prompt content) on failure. Workspace-
// specific path protection is checked by the native host before calling this.
func LoadProjectPrompt(p core.Project) (string, string) {
	if p.AppendSystemPrompt != "" && p.AppendSystemPromptFile != "" {
		return "", "configuration"
	}
	if p.AppendSystemPrompt != "" {
		if reason := projectPromptTextReason(p.AppendSystemPrompt); reason != "" {
			return "", reason
		}
		return p.AppendSystemPrompt, ""
	}
	if p.AppendSystemPromptFile == "" {
		return "", ""
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
	}
	return string(data), ""
}

func projectPromptTextReason(text string) string {
	switch {
	case len(text) > maxProjectPromptBytes:
		return "too_large"
	case !utf8.ValidString(text):
		return "invalid_utf8"
	case strings.ContainsRune(text, 0):
		return "nul"
	}
	return ""
}
