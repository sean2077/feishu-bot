package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

func (w *worker) control(ctx context.Context, r args) (args, error) {
	switch text(r["subtype"]) {
	case "can_use_tool":
		name, input := text(r["tool_name"]), object(r["input"])
		if input == nil {
			return nil, fmt.Errorf("invalid tool input")
		}
		kind := "permission"
		if name == "AskUserQuestion" {
			kind = "question"
		}
		answer := w.ask(ctx, kind, name, input)
		if truth(answer["approved"]) {
			updated := clone(input)
			if kind == "question" {
				updated["answers"] = answer["answers"]
			}
			return args{"behavior": "allow", "updatedInput": updated, "toolUseID": r["tool_use_id"]}, nil
		}
		message := "Operation not approved"
		if kind == "question" {
			message = "Question cancelled or expired"
		}
		return args{"behavior": "deny", "message": message, "toolUseID": r["tool_use_id"]}, nil
	case "hook_callback":
		return w.hook(ctx, text(r["callback_id"]), object(r["input"]))
	case "mcp_message":
		if r["server_name"] != "gateway" {
			return nil, fmt.Errorf("unknown MCP server")
		}
		message := object(r["message"])
		if message == nil {
			return nil, fmt.Errorf("invalid MCP message")
		}
		return args{"mcp_response": w.mcp(ctx, message)}, nil
	case "elicitation":
		return args{"action": "decline"}, nil
	default:
		return nil, fmt.Errorf("unsupported native control request: %s", text(r["subtype"]))
	}
}
func clone(in args) args {
	out := args{}
	for key, value := range in {
		out[key] = value
	}
	return out
}
func denyHook(reason string) args {
	return args{"hookSpecificOutput": args{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}}
}

type nativeFileScope struct{ work, home, inbound, rooms string }

// Resolve spelling/8.3 aliases once, before a native process can change files.
// Comparing a canonical tool target with a short-path root rejects legitimate
// Windows paths; resolving roots on every hook would instead follow later
// symlink replacements and silently enlarge the conversation's file scope.
func (w *worker) fileScope() (nativeFileScope, error) {
	w.scopeOnce.Do(func() {
		var root string
		root, w.scopeErr = physicalPath(w.workspace.Root)
		if w.scopeErr != nil {
			return
		}
		w.scope.rooms = filepath.Dir(root)
		w.scope.work, w.scopeErr = physicalPath(w.workspace.Work)
		if w.scopeErr != nil {
			return
		}
		w.scope.home, w.scopeErr = physicalPath(w.workspace.Home)
		if w.scopeErr != nil {
			return
		}
		if w.workspace.Inbound != "" {
			w.scope.inbound, w.scopeErr = physicalPath(w.workspace.Inbound)
			if w.scopeErr != nil {
				return
			}
			expected := filepath.Join(root, "inbound")
			if !core.Inside(expected, w.scope.inbound) || !core.Inside(w.scope.inbound, expected) {
				w.scopeErr = fmt.Errorf("conversation attachment directory was redirected")
			}
		}
	})
	return w.scope, w.scopeErr
}

func (w *worker) hook(ctx context.Context, id string, input args) (args, error) {
	switch id {
	case "compact":
		if input["hook_event_name"] == "PreCompact" {
			w.emit(core.WorkerEvent{Type: "compaction", Phase: "start", Trigger: text(input["trigger"])})
		}
		return args{}, nil
	case "stop":
		if input["hook_event_name"] == "Stop" {
			background := -1
			if tasks, ok := input["background_tasks"].([]any); ok {
				background = len(tasks)
			}
			event := core.WorkerEvent{Type: "activity", Background: &background}
			if crons, ok := input["session_crons"]; ok {
				pinned := len(array(crons)) > 0
				event.Pinned = &pinned
			}
			w.emit(event)
		}
		return args{}, nil
	case "question":
		if input["hook_event_name"] != "PreToolUse" || input["tool_name"] != "AskUserQuestion" {
			return args{}, nil
		}
		toolInput := object(input["tool_input"])
		answer := w.ask(ctx, "question", "AskUserQuestion", toolInput)
		if !truth(answer["approved"]) {
			return denyHook("Question cancelled or expired"), nil
		}
		updated := clone(toolInput)
		updated["answers"] = answer["answers"]
		return args{"hookSpecificOutput": args{"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": updated}}, nil
	case "tool":
		if input["hook_event_name"] != "PreToolUse" {
			return args{}, nil
		}
		name := text(input["tool_name"])
		w.emit(core.WorkerEvent{Type: "activity", Tool: name})
		scope, err := w.fileScope()
		if err != nil {
			return denyHook("Unable to verify conversation file scope"), nil
		}
		toolInput := object(input["tool_input"])
		keys := []string{"file_path", "notebook_path"}
		if name == "Grep" || name == "Glob" {
			keys = append(keys, "path")
		}
		for _, key := range keys {
			raw, ok := toolInput[key].(string)
			if !ok {
				continue
			}
			path := raw
			if !filepath.IsAbs(path) {
				path = filepath.Join(w.workspace.Work, path)
			}
			physical, err := physicalPath(path)
			if err != nil {
				return denyHook("Unable to verify conversation file scope"), nil
			}
			attachment := w.workspace.Inbound != "" && core.Inside(scope.inbound, physical)
			rooms := scope.rooms
			otherAttachment := false
			if core.Inside(rooms, physical) {
				relative, err := filepath.Rel(rooms, physical)
				if err == nil {
					parts := strings.Split(relative, string(filepath.Separator))
					otherAttachment = len(parts) >= 2 && strings.EqualFold(parts[1], "inbound") && !attachment
				}
			}
			if otherAttachment || w.workspace.NativeConfig != "inherit" && !core.Inside(scope.work, physical) && !core.Inside(scope.home, physical) && !attachment || attachment && name != "Read" && name != "Grep" && name != "Glob" {
				return denyHook("Outside conversation file scope or attempting to edit an incoming attachment"), nil
			}
		}
		return args{}, nil
	default:
		return nil, fmt.Errorf("unknown native hook callback")
	}
}
func physicalPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Abs(resolved)
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	physical, err := physicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(physical, filepath.Base(path)), nil
}

func stringSchema() args { return args{"type": "string"} }
func objectSchema(properties args, required ...string) args {
	schema := args{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
func (w *worker) tools() []args {
	list := []args{
		{"name": "read_chat_history", "description": "Read one bounded page of authorized messages from THIS conversation only when earlier context is needed. Never fetch all history automatically. Query timestamps use Unix seconds. Pass a returned cursor alone to continue; threadId must already be known in this chat.", "inputSchema": objectSchema(args{"limit": args{"type": "integer", "minimum": 1, "maximum": 50}, "startTime": args{"type": "integer", "minimum": 0}, "endTime": args{"type": "integer", "minimum": 0}, "cursor": args{"type": "string", "maxLength": 16000}, "threadId": args{"type": "string", "maxLength": 200}})},
		{"name": "fetch_attachment", "description": "Download an attachment from this conversation. Use IDs and file keys present in the input context.", "inputSchema": objectSchema(args{"messageId": stringSchema(), "key": stringSchema()}, "messageId", "key")},
		{"name": "send_file", "description": "Send a workspace file or echo an inbound attachment owned by this conversation. Other conversation and external paths are rejected.", "inputSchema": objectSchema(args{"path": stringSchema(), "name": stringSchema()}, "path")},
	}
	if len(w.project.Actions) > 0 {
		var actions []args
		for _, a := range w.project.Actions {
			actions = append(actions, args{"id": a.ID, "description": a.Description, "parameters": a.Parameters, "readOnly": a.ReadOnly})
		}
		encoded, _ := json.Marshal(actions)
		list = append([]args{{"name": "project_action", "description": "Execute a configured project operation. Operations and exact string parameters: " + string(encoded) + ". The host checks scope and approvals.", "inputSchema": objectSchema(args{"action": stringSchema(), "parameters": args{"type": "object", "additionalProperties": stringSchema(), "default": args{}}}, "action")}}, list...)
	}
	return list
}
func rpcResult(id any, result args) args { return args{"jsonrpc": "2.0", "id": id, "result": result} }
func rpcError(id any, code int, message string) args {
	return args{"jsonrpc": "2.0", "id": id, "error": args{"code": code, "message": message}}
}
func (w *worker) mcp(ctx context.Context, m args) args {
	id := m["id"]
	method := text(m["method"])
	if id == nil {
		return rpcResult(0, args{})
	}
	switch method {
	case "initialize":
		version := text(object(m["params"])["protocolVersion"])
		switch version {
		case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		default:
			version = "2025-11-25"
		}
		return rpcResult(id, args{"protocolVersion": version, "capabilities": args{"tools": args{}}, "serverInfo": args{"name": "gateway", "version": "0.1.0"}})
	case "ping":
		return rpcResult(id, args{})
	case "tools/list":
		return rpcResult(id, args{"tools": w.tools()})
	case "tools/call":
		params := object(m["params"])
		name := text(params["name"])
		input := object(params["arguments"])
		if input == nil {
			input = args{}
		}
		var kind, operation string
		var toolErr error
		switch name {
		case "read_chat_history":
			kind, operation = "history", name
			toolErr = validateHistory(input)
		case "fetch_attachment":
			kind, operation = "attachment", name
			toolErr = validateStrings(input, []string{"messageId", "key"}, nil)
		case "send_file":
			kind, operation = "send_file", name
			toolErr = validateStrings(input, []string{"path"}, []string{"name"})
		case "project_action":
			if len(w.project.Actions) == 0 {
				return rpcError(id, -32602, "Unknown tool")
			}
			kind, operation = "action", text(input["action"])
			if operation == "" {
				toolErr = fmt.Errorf("action must be a string")
			}
			if input["parameters"] == nil {
				input = args{}
			} else {
				input = object(input["parameters"])
				if input == nil {
					toolErr = fmt.Errorf("parameters must be an object")
				}
			}
			for _, value := range input {
				if _, ok := value.(string); !ok {
					toolErr = fmt.Errorf("parameters must contain strings")
				}
			}
		default:
			return rpcError(id, -32602, "Unknown tool")
		}
		if toolErr != nil {
			return rpcResult(id, args{"isError": true, "content": []args{{"type": "text", "text": toolErr.Error()}}})
		}
		result := w.ask(ctx, kind, operation, input)
		data, _ := json.Marshal(result)
		return rpcResult(id, args{"content": []args{{"type": "text", "text": string(data)}}})
	default:
		return rpcError(id, -32601, "Method not found")
	}
}
func validateStrings(input args, required, optional []string) error {
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
		if _, ok := input[key].(string); !ok {
			return fmt.Errorf("%s must be a string", key)
		}
	}
	for _, key := range optional {
		allowed[key] = true
		if value, exists := input[key]; exists {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s must be a string", key)
			}
		}
	}
	for key := range input {
		if !allowed[key] {
			return fmt.Errorf("unknown tool argument: %s", key)
		}
	}
	return nil
}
func validateHistory(input args) error {
	for key, value := range input {
		switch key {
		case "limit", "startTime", "endTime":
			n, ok := count(value)
			if !ok || key == "limit" && (n < 1 || n > 50) {
				return fmt.Errorf("invalid history %s", key)
			}
		case "cursor", "threadId":
			s, ok := value.(string)
			max := 16000
			if key == "threadId" {
				max = 200
			}
			if !ok || len([]rune(s)) > max {
				return fmt.Errorf("invalid history %s", key)
			}
		default:
			return fmt.Errorf("unknown history argument: %s", key)
		}
	}
	return nil
}
