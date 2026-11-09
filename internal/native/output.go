package native

import (
	"fmt"
	"math"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

type args = core.Args

func object(v any) args {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
func text(v any) string { s, _ := v.(string); return s }
func array(v any) []any { s, _ := v.([]any); return s }
func truth(v any) bool  { b, _ := v.(bool); return b }
func has(v any) bool    { return v != nil && v != "" && v != false }
func stringsOf(v any) []string {
	var out []string
	for _, x := range array(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func count(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 && x <= 9007199254740991 && math.Trunc(x) == x {
			return int64(x), true
		}
	case int:
		if x >= 0 {
			return int64(x), true
		}
	case int64:
		if x >= 0 {
			return x, true
		}
	}
	return 0, false
}

type outputBlock struct {
	index   int
	kind    string
	matched bool
}
type nativeOutput struct {
	current                      string
	sequence                     int
	anchor, source, backgroundID string
	tasks                        map[string]string
	taskOrder                    []string
	blocks                       map[string][]*outputBlock
	blockOrder                   []string
	known                        func(string) bool
}

func newOutput(known func(string) bool) *nativeOutput {
	return &nativeOutput{source: "unattributed", tasks: map[string]string{}, blocks: map[string][]*outputBlock{}, known: known}
}
func (o *nativeOutput) stamp(e args) string {
	origin := object(e["origin"])
	if origin != nil && origin["kind"] == "task-notification" && text(origin["runId"]) != "" {
		return o.tasks[text(origin["runId"])]
	}
	if origin != nil && text(origin["senderTaskId"]) != "" {
		return o.tasks[text(origin["senderTaskId"])]
	}
	if origin != nil && origin["kind"] != "human" {
		return ""
	}
	ids := stringsOf(e["user_message_uuids"])
	if len(ids) == 0 {
		ids = []string{text(e["user_message_uuid"])}
		if e["type"] == "user" {
			ids = append(ids, text(e["uuid"]))
		}
	}
	for _, id := range ids {
		if id != "" && o.known(id) {
			return id
		}
	}
	return ""
}
func (o *nativeOutput) origin(e args) string {
	stamp := o.stamp(e)
	origin := object(e["origin"])
	if origin != nil && origin["kind"] != "human" {
		return stamp
	}
	if stamp != "" {
		return stamp
	}
	return o.anchor
}
func (o *nativeOutput) eventSource(e args) string {
	origin := object(e["origin"])
	if origin != nil && (origin["kind"] != "human" || text(origin["senderTaskId"]) != "") {
		return "background"
	}
	if o.stamp(e) != "" || origin != nil && origin["kind"] == "human" {
		return "foreground"
	}
	return o.source
}
func (o *nativeOutput) background(e args) string {
	origin := object(e["origin"])
	if origin != nil {
		if id := text(origin["runId"]); id != "" {
			return id
		}
		return text(origin["senderTaskId"])
	}
	return o.backgroundID
}
func (o *nativeOutput) end() {
	o.current = ""
	o.anchor = ""
	o.source = "unattributed"
	o.backgroundID = ""
	o.blocks = map[string][]*outputBlock{}
	o.blockOrder = nil
}
func (o *nativeOutput) handle(e args) []core.WorkerEvent {
	if has(e["parent_tool_use_id"]) {
		return nil
	}
	stamp := o.stamp(e)
	typ := text(e["type"])
	// Native Claude Code folds a task notification that completes during a
	// running foreground turn into that turn and replays it as a user message;
	// the rest of the turn and its result still answer the human input. When
	// that same input started the task, the output stays on its reply card.
	// A task from an earlier input keeps its own attribution.
	absorbed := typ == "user" && o.source == "foreground" && o.anchor != "" && stamp == o.anchor && o.eventSource(e) == "background"
	if !absorbed && (typ == "assistant" || typ == "stream_event" || typ == "result" || typ == "user" && (e["origin"] != nil || stamp != "")) {
		o.source = o.eventSource(e)
		if o.source == "background" {
			o.backgroundID = o.background(e)
		} else {
			o.backgroundID = ""
		}
		if stamp != "" {
			o.anchor = stamp
		} else if origin := object(e["origin"]); origin != nil && origin["kind"] != "human" {
			o.anchor = ""
		}
	}
	if typ == "system" && e["subtype"] == "task_started" {
		anchor := o.anchor
		if parent, ok := e["parent_task_id"].(string); ok {
			anchor = o.tasks[parent]
		}
		if anchor != "" {
			for _, key := range []string{"task_id", "run_id"} {
				if id := text(e[key]); id != "" {
					if _, ok := o.tasks[id]; !ok {
						o.taskOrder = append(o.taskOrder, id)
					}
					o.tasks[id] = anchor
				}
			}
		}
		for len(o.taskOrder) > 10000 {
			delete(o.tasks, o.taskOrder[0])
			o.taskOrder = o.taskOrder[1:]
		}
	}
	var out []core.WorkerEvent
	send := func(kind string, v any, partial bool, id string) {
		s, ok := v.(string)
		if (kind == "text" || kind == "thinking") && ok {
			out = append(out, core.WorkerEvent{Type: kind, Text: s, Partial: partial, BlockID: id, TurnID: o.origin(e), Source: o.source, BackgroundID: o.backgroundID})
		}
	}
	if typ == "stream_event" {
		d := object(e["event"])
		if d == nil {
			return out
		}
		if d["type"] == "message_start" {
			o.current = text(object(d["message"])["id"])
			if o.current == "" {
				o.sequence++
				o.current = fmt.Sprintf("stream-%d", o.sequence)
			}
			if _, ok := o.blocks[o.current]; !ok {
				o.blocks[o.current] = nil
				o.blockOrder = append(o.blockOrder, o.current)
			}
		}
		n, _ := count(d["index"])
		index := int(n)
		id := fmt.Sprintf("%s:%d", o.current, index)
		if d["type"] == "content_block_start" {
			b := object(d["content_block"])
			if b != nil {
				known := o.blocks[o.current]
				exists := false
				for _, k := range known {
					if k.index == index {
						exists = true
					}
				}
				if !exists {
					o.blocks[o.current] = append(known, &outputBlock{index: index, kind: text(b["type"])})
				}
				v := b["text"]
				if b["type"] == "thinking" {
					v = b["thinking"]
				}
				send(text(b["type"]), v, false, id)
			}
		}
		if d["type"] == "content_block_delta" {
			delta := object(d["delta"])
			if delta["type"] == "text_delta" {
				send("text", delta["text"], true, id)
			}
			if delta["type"] == "thinking_delta" {
				send("thinking", delta["thinking"], true, id)
			}
		}
	} else if typ == "assistant" {
		m := object(e["message"])
		content := array(m["content"])
		id := text(m["id"])
		if id == "" {
			id = o.current
		}
		if id == "" {
			o.sequence++
			id = fmt.Sprintf("message-%d", o.sequence)
		}
		known := o.blocks[id]
		for i, raw := range content {
			b := object(raw)
			kind := text(b["type"])
			var match *outputBlock
			if len(content) > 1 {
				for _, k := range known {
					if k.index == i && k.kind == kind {
						match = k
						break
					}
				}
			}
			if match == nil {
				for _, k := range known {
					if !k.matched && k.kind == kind {
						match = k
						break
					}
				}
			}
			if match == nil {
				for _, k := range known {
					if k.kind == kind {
						match = k
					}
				}
			}
			index := i
			if match != nil {
				match.matched = true
				index = match.index
			}
			v := b["text"]
			if kind == "thinking" {
				v = b["thinking"]
			}
			send(kind, v, false, fmt.Sprintf("%s:%d", id, index))
		}
	}
	for len(o.blockOrder) > 256 {
		delete(o.blocks, o.blockOrder[0])
		o.blockOrder = o.blockOrder[1:]
	}
	return out
}
func resultOrigin(e args, known map[string]bool, anchor, source string) (string, []string) {
	var ids []string
	raw, ok := e["user_message_uuids"].([]any)
	if !ok {
		raw = []any{e["user_message_uuid"]}
	}
	uuid := text(e["user_message_uuid"])
	if !known[uuid] {
		uuid = ""
	}
	for _, v := range raw {
		if id := text(v); known[id] {
			ids = append(ids, id)
		}
	}
	origin := object(e["origin"])
	foreground := source != "background" && !has(e["parent_tool_use_id"]) && (origin == nil || origin["kind"] == "human" && text(origin["senderTaskId"]) == "")
	if uuid == "" && len(ids) == 0 && foreground && known[anchor] {
		uuid = anchor
		ids = append(ids, anchor)
	}
	return uuid, ids
}

type contextSnapshot struct {
	model, message    string
	input             int64
	haveInput, stream bool
}

func inputSize(v any) (int64, bool) {
	u := object(v)
	input, ok := count(u["input_tokens"])
	if !ok {
		return 0, false
	}
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v, exists := u[key]; exists {
			n, valid := count(v)
			if !valid {
				return 0, false
			}
			input += n
			if input > 9007199254740991 {
				return 0, false
			}
		}
	}
	return input, true
}
func (c *contextSnapshot) observe(e args) {
	if has(e["parent_tool_use_id"]) {
		return
	}
	if e["type"] == "system" && e["subtype"] == "compact_boundary" {
		*c = contextSnapshot{}
		return
	}
	stream := object(e["event"])
	start := e["type"] == "stream_event" && stream["type"] == "message_start"
	var m args
	if start {
		m = object(stream["message"])
	} else if e["type"] == "assistant" {
		m = object(e["message"])
	}
	if m == nil {
		return
	}
	id := text(m["id"])
	if start || id != c.message {
		c.model = text(m["model"])
		c.message = id
		c.input, c.haveInput = inputSize(m["usage"])
		c.stream = start && c.haveInput
	} else if !c.stream {
		if model, ok := m["model"].(string); ok {
			c.model = model
		}
		if size, ok := inputSize(m["usage"]); ok {
			c.input = size
			c.haveInput = true
		}
	}
}
func nativeModel(name string) string {
	if strings.HasSuffix(strings.ToLower(name), "[1m]") {
		return name[:len(name)-4]
	}
	return name
}
func requestKey(name string) string {
	if strings.HasSuffix(strings.ToLower(name), "[1m]") {
		return name[:len(name)-4] + "[1m]"
	}
	return name
}
func (c *contextSnapshot) result(e args, requested string) args {
	rows := object(e["modelUsage"])
	var row args
	if c.model != "" && rows != nil {
		var selected []string
		if requested != "" && nativeModel(requested) == c.model {
			for name := range rows {
				if requestKey(name) == requestKey(requested) {
					selected = append(selected, name)
				}
			}
		}
		if len(selected) == 1 {
			row = object(rows[selected[0]])
		} else if len(selected) == 0 {
			if raw, ok := rows[c.model]; ok {
				row = object(raw)
			} else {
				var matches []string
				for name := range rows {
					if nativeModel(name) == c.model {
						matches = append(matches, name)
					}
				}
				if len(matches) == 1 {
					row = object(rows[matches[0]])
				}
			}
		}
	}
	result := args{}
	if c.haveInput {
		result["inputTokens"] = c.input
	}
	if window, ok := count(row["contextWindow"]); ok && window > 0 {
		result["windowTokens"] = window
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
