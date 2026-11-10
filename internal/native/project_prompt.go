package native

import (
	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

// loadProjectPrompt adds native-workspace path protection and then uses the
// shared config loader. Prompt failures are fail-open in the worker.
func loadProjectPrompt(p core.Project, w core.Workspace) (text, reason string) {
	if p.AppendSystemPromptFile != "" && ((w.Home != "" && core.Inside(w.Home, p.AppendSystemPromptFile)) || (w.Root != "" && core.Inside(w.Root, p.AppendSystemPromptFile))) {
		return "", "path"
	}
	return config.LoadProjectPrompt(p)
}

func projectPromptText(prompt string) string {
	if prompt == "" {
		return ""
	}
	return "Project instructions:\n" + prompt
}
