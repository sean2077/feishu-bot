package native

import (
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestHistoryToolAdvertisesChatScopeAndBackgroundSemantics(t *testing.T) {
	w := &worker{project: core.Project{}}
	var history args
	for _, tool := range w.tools() {
		if tool["name"] == "read_chat_history" {
			history = tool
		}
	}
	if history == nil {
		t.Fatal("read_chat_history was not registered")
	}
	schema := object(history["inputSchema"])
	properties := object(schema["properties"])
	scope := object(properties["scope"])
	if !strings.Contains(text(history["description"]), "scope=chat") || !strings.Contains(text(scope["type"]), "string") {
		t.Fatalf("history tool does not advertise chat scope: %+v", history)
	}
	if _, ok := properties["expandThreads"]; !ok {
		t.Fatal("history tool does not advertise expandThreads")
	}
	prompt := text(initialization(core.Project{}, 1000, "", nil)["appendSystemPrompt"])
	if !strings.Contains(prompt, "background:true") || !strings.Contains(prompt, "not requests") {
		t.Fatalf("bridge prompt lacks background semantics: %q", prompt)
	}
}
