package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

// The history tool must survive an isolated project settings directory. It is
// part of the fixed allowed tools and the gateway MCP server, not something a
// project settings file has to opt into.
func TestIsolatedProjectRegistersReadChatHistory(t *testing.T) {
	p := core.Project{ID: "project", PermissionMode: "default"}
	args := launchArgs(core.Config{}, p, core.Session{}, core.Workspace{NativeConfig: "isolated"}, "")
	var allowed string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--allowedTools" {
			allowed = args[i+1]
		}
	}
	found := false
	for _, tool := range strings.Split(allowed, ",") {
		if tool == "mcp__gateway__read_chat_history" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fixed allowed tools do not include read_chat_history: %q", allowed)
	}
	servers, _ := initialization(p, 300000)["sdkMcpServers"].([]string)
	if len(servers) != 1 || servers[0] != "gateway" {
		t.Fatalf("gateway MCP server is not registered: %v", servers)
	}
	w := &worker{}
	listed := false
	for _, tool := range w.tools() {
		if tool["name"] == "read_chat_history" {
			listed = true
		}
	}
	if !listed {
		t.Fatal("gateway MCP server does not advertise read_chat_history")
	}
}

// The tool has no chat parameter: scope comes from the room bound to the
// native process. Rejecting unknown arguments keeps a model from asking for a
// different chat, even if a later gateway check would also refuse it.
func TestReadChatHistoryRejectsCrossChatArguments(t *testing.T) {
	w := &worker{}
	response := w.mcp(context.Background(), args{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": args{"name": "read_chat_history", "arguments": args{"chatId": "oc_other"}}})
	result := object(response["result"])
	if result["isError"] != true {
		t.Fatalf("cross-chat argument was not rejected: %v", response)
	}
	encoded, _ := json.Marshal(response)
	if !strings.Contains(string(encoded), "unknown history argument: chatId") {
		t.Fatalf("unexpected rejection: %v", response)
	}
}
