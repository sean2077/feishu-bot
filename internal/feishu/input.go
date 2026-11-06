package feishu

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

type identity struct {
	OpenID  string `json:"open_id"`
	UserID  string `json:"user_id"`
	UnionID string `json:"union_id"`
}
type mention struct {
	Key  string   `json:"key"`
	ID   identity `json:"id"`
	Name string   `json:"name"`
}
type eventMessage struct {
	ID         string    `json:"message_id"`
	ChatID     string    `json:"chat_id"`
	ChatType   string    `json:"chat_type"`
	Type       string    `json:"message_type"`
	Content    string    `json:"content"`
	CreateTime string    `json:"create_time"`
	ParentID   string    `json:"parent_id"`
	RootID     string    `json:"root_id"`
	ThreadID   string    `json:"thread_id"`
	Mentions   []mention `json:"mentions"`
}
type messageEvent struct {
	Sender struct {
		ID   identity `json:"sender_id"`
		Type string   `json:"sender_type"`
		Name string   `json:"sender_name"`
	} `json:"sender"`
	Message eventMessage `json:"message"`
}

const invalidInput = "消息内容或资源信息缺失，无法完整解析。请重新发送原文或附件。"
const unsupportedInput = "这种消息内容尚不支持完整解析。请改发普通文本、图片或文件。"
const forwardedInput = "合并转发不会自动展开。请在当前聊天发送获准使用的原始消息或附件。"
const folderInput = "文件夹引用不是可下载附件，请发送需要处理的具体文件。"

func member(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func roomFor(c core.Config, chat, dm string) (core.Room, bool) {
	if !validID(chat) {
		return core.Room{}, false
	}
	return c.RoomFor(chat, dm)
}
func (c *Client) validatedRoom(room core.Room) (core.Room, error) {
	var parts []any
	if json.Unmarshal([]byte(room.Key), &parts) != nil || len(parts) != 6 {
		return core.Room{}, fmt.Errorf("feishu: invalid room scope")
	}
	chat, ok := parts[4].(string)
	if !ok || chat != room.ChatID {
		return core.Room{}, fmt.Errorf("feishu: invalid room scope")
	}
	kind, _ := parts[3].(string)
	dm := ""
	if kind == "p2p" {
		dm, ok = parts[5].(string)
		if !ok || dm == "" {
			return core.Room{}, fmt.Errorf("feishu: invalid room scope")
		}
	} else if kind != "group" {
		return core.Room{}, fmt.Errorf("feishu: invalid room scope")
	}
	current, ok := roomFor(c.config, chat, dm)
	if !ok || current.Key != room.Key {
		return core.Room{}, fmt.Errorf("feishu: invalid room scope")
	}
	return current, nil
}

// approvalRecipient validates the gateway's explicit maintainer-forward target
// against the original configured DM, without trusting caller-supplied roles.
func (c *Client) approvalRecipient(target core.Room) (string, error) {
	var key []any
	if json.Unmarshal([]byte(target.Key), &key) != nil || len(key) != 6 {
		return "", fmt.Errorf("feishu: invalid approval destination")
	}
	chat, ok := key[4].(string)
	if !ok || !validID(target.ChatID) || target.ChatID != target.DMUser {
		return "", fmt.Errorf("feishu: invalid approval destination")
	}
	origin := target
	origin.ChatID = chat
	current, err := c.validatedRoom(origin)
	if err != nil || current.DMUser == "" || member(current.Maintainers, current.DMUser) || !member(current.Maintainers, target.DMUser) {
		return "", fmt.Errorf("feishu: invalid approval destination")
	}
	return target.DMUser, nil
}
func (c *Client) configuredMaintainer(id string) bool {
	for _, p := range c.config.Projects {
		if member(p.Maintainers, id) {
			return true
		}
	}
	return false
}

func authorizedAuthor(room core.Room, sender string) bool {
	return room.Authorizes(sender)
}
func (c *Client) allowedHeader(event messageEvent) (core.Room, bool) {
	m := event.Message
	sender := event.Sender.ID.OpenID
	if !validID(m.ID) || !validID(m.ChatID) || !validID(sender) || event.Sender.Type != "user" {
		return core.Room{}, false
	}
	dm := ""
	if m.ChatType == "p2p" {
		dm = sender
	} else if m.ChatType != "group" {
		return core.Room{}, false
	}
	room, ok := roomFor(c.config, m.ChatID, dm)
	return room, ok && authorizedAuthor(room, sender)
}
func botMentioned(m eventMessage, botID string) bool {
	if botID == "" {
		return false
	}
	for _, v := range m.Mentions {
		if v.ID.OpenID == botID {
			return true
		}
	}
	return false
}

// NormalizeEvent is a pure, bounded provider boundary. It keeps neither the raw
// event nor card action values. Unknown content is visibly rejected, not partly
// presented as a complete message. Caller policy runs before this renderer.
func NormalizeEvent(raw []byte, botID string) (core.Message, error) {
	if len(raw) > 2<<20 {
		return core.Message{}, fmt.Errorf("feishu: event exceeds limit")
	}
	var event messageEvent
	if json.Unmarshal(raw, &event) != nil {
		return core.Message{}, fmt.Errorf("feishu: malformed event")
	}
	return normalizeMessage(event, botID, true), nil
}
func normalizeMessage(event messageEvent, botID string, strip bool) core.Message {
	raw := event.Message
	timestamp, _ := strconv.ParseInt(raw.CreateTime, 10, 64)
	m := core.Message{ID: raw.ID, ChatID: raw.ChatID, ChatType: raw.ChatType, SenderID: event.Sender.ID.OpenID, SenderName: event.Sender.Name, SenderIsBot: event.Sender.Type != "user", Text: "", Time: timestamp, Mentioned: botMentioned(raw, botID), ReplyTo: raw.ParentID, ThreadID: raw.ThreadID, Resources: []core.Resource{}}
	issue := ""
	if raw.Type == "merge_forward" {
		issue = forwardedInput
	} else if len(raw.Content) > 1<<20 {
		issue = invalidInput
	} else {
		var content map[string]any
		if json.Unmarshal([]byte(raw.Content), &content) != nil || content == nil {
			issue = invalidInput
		} else {
			// Post mentions are rendered as "@_user_1" keys or display names in
			// the content body, not necessarily as open_ids. Strip every key and
			// name that the event's mention list attributes to this bot.
			botMentions := map[string]bool{}
			if strip && m.Mentioned {
				for _, v := range raw.Mentions {
					if botID != "" && v.ID.OpenID == botID {
						if v.Key != "" {
							botMentions[v.Key] = true
						}
						if v.Name != "" {
							botMentions[v.Name] = true
							botMentions["@"+v.Name] = true
						}
					}
				}
			}
			m.Text, m.Resources, issue = renderContent(raw.Type, content, botID, strip && m.Mentioned, botMentions)
		}
	}
	if issue == "" {
		mentions := append([]mention(nil), raw.Mentions...)
		sort.SliceStable(mentions, func(i, j int) bool { return len(mentions[i].Key) > len(mentions[j].Key) })
		for _, v := range mentions {
			if v.Key == "" {
				continue
			}
			if strip && m.Mentioned && v.ID.OpenID == botID {
				m.Text = strings.ReplaceAll(m.Text, v.Key, "")
			} else if v.Name != "" {
				m.Text = strings.ReplaceAll(m.Text, v.Key, "@"+v.Name)
			}
		}
		if strip && m.Mentioned {
			m.Text = strings.TrimSpace(m.Text)
		}
	}
	if len(m.Resources) > 12 {
		issue = invalidInput
	}
	if issue != "" {
		m.InputIssue = issue
		m.Text = "[内容未完整解析：" + issue + "]"
		m.Resources = []core.Resource{}
	}
	m.ImageOnly = issue == "" && raw.Type == "image" && len(m.Resources) > 0
	return m
}
func resource(kind, key, name string) core.Resource {
	if name == "" {
		suffix := key
		if len(suffix) > 12 {
			suffix = suffix[len(suffix)-12:]
		}
		name = kind + "-" + suffix
	}
	return core.Resource{Kind: kind, Key: key, Name: name}
}
func resourceFields(a map[string]any, key string) (string, string, bool) {
	k, ok := a[key].(string)
	if !ok || !validID(k) {
		return "", "", false
	}
	name := ""
	if v, exists := a["file_name"]; exists {
		name, ok = v.(string)
		if !ok || len(name) > 4096 {
			return "", "", false
		}
	}
	return k, name, true
}
func renderContent(kind string, content map[string]any, botID string, strip bool, botMentions map[string]bool) (string, []core.Resource, string) {
	switch kind {
	case "text":
		s, ok := content["text"].(string)
		if !ok {
			return "", nil, invalidInput
		}
		return s, nil, ""
	case "image":
		key, name, ok := resourceFields(content, "image_key")
		if !ok {
			return "", nil, invalidInput
		}
		return "[图片]", []core.Resource{resource("image", key, name)}, ""
	case "file", "audio", "video", "media":
		key, name, ok := resourceFields(content, "file_key")
		if !ok {
			return "", nil, invalidInput
		}
		r := resource("file", key, name)
		return "[文件：" + r.Name + "]", []core.Resource{r}, ""
	case "post":
		return renderPost(content, botID, strip, botMentions)
	case "interactive":
		text, ok := renderInteractive(content)
		if !ok {
			return "", nil, unsupportedInput
		}
		return text, nil, ""
	default:
		return "", nil, unsupportedInput
	}
}
func object(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
func renderPost(content map[string]any, botID string, strip bool, botMentions map[string]bool) (string, []core.Resource, string) {
	var body map[string]any
	if _, ok := content["title"]; ok {
		body = content
	} else if _, ok := content["content"]; ok {
		body = content
	} else if _, ok := content["content_v2"]; ok {
		body = content
	} else {
		for _, locale := range []string{"zh_cn", "en_us", "ja_jp"} {
			if v, ok := object(content[locale]); ok {
				body = v
				break
			}
		}
		if body == nil {
			keys := make([]string, 0, len(content))
			for k := range content {
				if k != "files" {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				if v, ok := object(content[k]); ok {
					body = v
					break
				}
			}
		}
	}
	var files []any
	if value, ok := content["files"]; ok {
		var valid bool
		files, valid = value.([]any)
		if !valid || len(files) > 12 {
			return "", nil, invalidInput
		}
	}
	if body == nil && len(files) == 0 {
		return "", nil, invalidInput
	}
	lines := []string{}
	resources := []core.Resource{}
	if v, ok := body["title"]; ok {
		title, valid := v.(string)
		if !valid {
			return "", nil, invalidInput
		}
		if title != "" {
			lines = append(lines, title)
		}
	}
	var paragraphs []any
	if v, ok := body["content_v2"].([]any); ok && len(v) > 0 {
		paragraphs = v
	} else if value, ok := body["content"]; ok {
		var valid bool
		paragraphs, valid = value.([]any)
		if !valid {
			return "", nil, invalidInput
		}
	}
	count := 0
	for _, p := range paragraphs {
		elements, ok := p.([]any)
		if !ok {
			return "", nil, invalidInput
		}
		var line strings.Builder
		for _, value := range elements {
			count++
			if count > 4096 {
				return "", nil, invalidInput
			}
			el, ok := object(value)
			if !ok {
				return "", nil, invalidInput
			}
			for _, key := range []string{"text", "href", "user_id", "user_name", "language"} {
				if v, exists := el[key]; exists {
					if _, ok := v.(string); !ok {
						return "", nil, invalidInput
					}
				}
			}
			if value, exists := el["style"]; exists {
				styles, ok := value.([]any)
				if !ok {
					return "", nil, invalidInput
				}
				for _, s := range styles {
					if _, ok := s.(string); !ok {
						return "", nil, invalidInput
					}
				}
			}
			switch stringValue(el, "tag") {
			case "text", "md":
				line.WriteString(stringValue(el, "text"))
			case "a":
				text, href := stringValue(el, "text"), stringValue(el, "href")
				if text == "" {
					text = href
				}
				if href == "" {
					line.WriteString(text)
				} else {
					line.WriteString("[" + text + "](" + href + ")")
				}
			case "at":
				id := stringValue(el, "user_id")
				if strip && (id == botID || botMentions[id]) {
					continue
				}
				name := stringValue(el, "user_name")
				if name == "" {
					name = id
				}
				line.WriteString("@" + name)
			case "img", "media":
				keyName, kind := "image_key", "image"
				if stringValue(el, "tag") == "media" {
					keyName, kind = "file_key", "file"
				}
				key, name, ok := resourceFields(el, keyName)
				if !ok {
					return "", nil, invalidInput
				}
				r := resource(kind, key, name)
				resources = append(resources, r)
				if kind == "image" {
					line.WriteString("[图片]")
				} else {
					line.WriteString("[文件：" + r.Name + "]")
				}
			case "code_block":
				line.WriteString("\n```" + stringValue(el, "language") + "\n" + stringValue(el, "text") + "\n```\n")
			case "hr":
				line.WriteString("\n---\n")
			default:
				return "", nil, unsupportedInput
			}
			if len(resources) > 12 {
				return "", nil, invalidInput
			}
		}
		lines = append(lines, line.String())
	}
	for _, value := range files {
		el, ok := object(value)
		if !ok {
			return "", nil, invalidInput
		}
		key, name, valid := resourceFields(el, "file_key")
		if !valid {
			return "", nil, invalidInput
		}
		if el["is_folder"] == true {
			return "", nil, folderInput
		}
		r := resource("file", key, name)
		resources = append(resources, r)
		lines = append(lines, "[文件："+r.Name+"]")
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" || len(resources) > 12 {
		return "", nil, invalidInput
	}
	return text, resources, ""
}

// Interactive cards are context only. Extract visible text, never callback
// values, URLs from arbitrary fields, signatures, or hidden application state.
func renderInteractive(content map[string]any) (string, bool) {
	var texts []string
	nodes := 0
	valid := true
	var visit func(any, int)
	visit = func(value any, depth int) {
		nodes++
		if depth > 24 || nodes > 4096 {
			valid = false
			return
		}
		switch v := value.(type) {
		case []any:
			for _, child := range v {
				visit(child, depth+1)
			}
		case map[string]any:
			tag := stringValue(v, "tag")
			if tag == "markdown" || tag == "plain_text" || tag == "lark_md" {
				if s, ok := v["content"].(string); ok && s != "" {
					texts = append(texts, s)
				}
			}
			// Old message-card history may flatten text as {"tag":"text","text":...}.
			if tag == "text" {
				if s, ok := v["text"].(string); ok && s != "" {
					texts = append(texts, s)
				}
			}
			for _, key := range []string{"header", "title", "body", "elements", "text", "fields", "columns", "extra"} {
				if child, exists := v[key]; exists {
					visit(child, depth+1)
				}
			}
		}
	}
	visit(content, 0)
	return strings.Join(texts, "\n"), valid
}
