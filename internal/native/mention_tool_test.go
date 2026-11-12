package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func mentionTool(room core.Room) (args, bool) {
	w := &worker{room: room}
	for _, tool := range w.tools() {
		if tool["name"] == "mention_bot" {
			return tool, true
		}
	}
	return nil, false
}

func TestMentionBotToolRequiresAllowOtherBots(t *testing.T) {
	bot := core.OtherBot{AppID: "cli_helper", Name: "Helper", OpenID: "ou_helper"}
	if _, ok := mentionTool(core.Room{OtherBots: []core.OtherBot{bot}}); ok {
		t.Fatal("mention_bot was advertised without allowOtherBots")
	}
	if _, ok := mentionTool(core.Room{AllowOtherBots: true, OtherBots: []core.OtherBot{{AppID: "cli_helper", Name: "Helper"}}}); ok {
		t.Fatal("mention_bot was advertised without a resolvable openId")
	}
	tool, ok := mentionTool(core.Room{AllowOtherBots: true, OtherBots: []core.OtherBot{bot}})
	if !ok || !strings.Contains(text(tool["description"]), "Helper") {
		t.Fatalf("mention_bot tool = %+v %t", tool, ok)
	}
	w := &worker{}
	response := w.mcp(context.Background(), args{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": args{"name": "mention_bot", "arguments": args{"target": "Helper"}}})
	if response["error"] == nil {
		t.Fatalf("mention_bot was callable without allowOtherBots: %v", response)
	}
}

func TestMentionBotAllowedTools(t *testing.T) {
	args := launchArgs(core.Config{}, core.Project{ID: "project", PermissionMode: "default"}, core.Session{}, core.Workspace{NativeConfig: "isolated"}, "")
	allowed := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--allowedTools" {
			allowed = args[i+1]
		}
	}
	if !strings.Contains(allowed, "mcp__gateway__mention_bot") {
		t.Fatalf("mention_bot missing from allowed tools: %q", allowed)
	}
}

func TestMentionBotRejectsUnknownArguments(t *testing.T) {
	w := &worker{room: core.Room{AllowOtherBots: true, OtherBots: []core.OtherBot{{AppID: "cli_helper", Name: "Helper", OpenID: "ou_helper"}}}}
	response := w.mcp(context.Background(), args{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": args{"name": "mention_bot", "arguments": args{"target": "Helper", "appId": "cli_other"}}})
	encoded, _ := json.Marshal(response)
	if object(response["result"])["isError"] != true || !strings.Contains(string(encoded), "unknown tool argument") {
		t.Fatalf("unknown mention_bot argument was accepted: %v", response)
	}
}
