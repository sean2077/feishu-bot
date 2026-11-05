package gateway

import (
	"encoding/json"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
)

func RoomFor(c core.Config, chat, dm string) (core.Room, bool) {
	project := ""
	var members []string
	mentionOptional := false
	if dm != "" {
		for _, b := range c.DMs {
			if b.UserID == dm {
				project = b.ProjectID
				break
			}
		}
	} else {
		for _, b := range c.Groups {
			if b.ChatID == chat {
				project = b.ProjectID
				members = b.Members
				mentionOptional = b.MentionOptional
				break
			}
		}
	}
	p, ok := c.Project(project)
	if !ok || project == "" {
		return core.Room{}, false
	}
	kind := "group"
	var user any
	if dm != "" {
		kind = "p2p"
		user = dm
	}
	key, _ := json.Marshal([]any{c.TenantID, c.AppID, project, kind, chat, user})
	return core.Room{Key: string(key), ChatID: chat, ProjectID: project, DMUser: dm, Maintainers: p.Maintainers, Members: members, MentionOptional: mentionOptional}, true
}
func RoomFromKey(c core.Config, key string) (core.Room, bool) {
	var a []any
	if json.Unmarshal([]byte(key), &a) != nil || len(a) != 6 {
		return core.Room{}, false
	}
	chat, ok := a[4].(string)
	if !ok {
		return core.Room{}, false
	}
	kind, _ := a[3].(string)
	if kind != "group" && kind != "p2p" {
		return core.Room{}, false
	}
	dm := ""
	if kind == "p2p" {
		dm, ok = a[5].(string)
		if !ok || dm == "" {
			return core.Room{}, false
		}
	}
	r, ok := RoomFor(c, chat, dm)
	return r, ok && r.Key == key
}
func contains(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}
func AuthorizedAuthor(r core.Room, id string) bool {
	if id == "" {
		return false
	}
	if r.DMUser != "" {
		return r.DMUser == id
	}
	return contains(r.Maintainers, id) || contains(r.Members, id)
}
func Route(c core.Config, m core.Message) (core.Room, bool) {
	if m.SenderIsBot || m.SenderID == "" || m.ChatID == "" || m.ID == "" || (m.ChatType != "group" && m.ChatType != "p2p") {
		return core.Room{}, false
	}
	dm := ""
	if m.ChatType == "p2p" {
		dm = m.SenderID
	}
	r, ok := RoomFor(c, m.ChatID, dm)
	return r, ok && AuthorizedAuthor(r, m.SenderID)
}
func Triggers(r core.Room, m core.Message) bool {
	return m.ChatType == "p2p" || m.Mentioned || r.MentionOptional
}
func Readable(r core.Room, m core.Message) bool {
	kind := "group"
	if r.DMUser != "" {
		kind = "p2p"
	}
	if m.ID == "" || m.ChatID != r.ChatID || m.ChatType != kind {
		return false
	}
	if m.SenderIsBot {
		return m.SenderIsSelf
	}
	return AuthorizedAuthor(r, m.SenderID)
}
func canManage(r core.Room, id string) bool { return r.DMUser == id || contains(r.Maintainers, id) }
func cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	count := 0
	for at := range s {
		if count == n {
			return s[:at]
		}
		count++
	}
	return s
}
func jsonText(v any) string                  { b, _ := json.Marshal(v); return string(b) }
func stringArg(a core.Args, k string) string { s, _ := a[k].(string); return s }
func boolArg(a core.Args, k string) bool     { v, _ := a[k].(bool); return v }
func numberArg(a core.Args, k string) (int64, bool) {
	switch v := a[k].(type) {
	case float64:
		if v >= 0 && v == float64(int64(v)) {
			return int64(v), true
		}
	case int:
		if v >= 0 {
			return int64(v), true
		}
	case int64:
		if v >= 0 {
			return v, true
		}
	case json.Number:
		n, e := v.Int64()
		return n, e == nil && n >= 0
	}
	return 0, false
}
func argsOf(v any) core.Args {
	if a, ok := v.(map[string]any); ok {
		return a
	}
	b, _ := json.Marshal(v)
	var a core.Args
	_ = json.Unmarshal(b, &a)
	if a == nil {
		a = core.Args{}
	}
	return a
}
func origins(ds []core.Delivery) []core.Origin {
	o := make([]core.Origin, 0, len(ds))
	for _, d := range ds {
		o = append(o, core.Origin{ID: d.Message.ID, SenderID: d.Message.SenderID})
	}
	return o
}
func origin(m core.Message) []core.Origin { return []core.Origin{{ID: m.ID, SenderID: m.SenderID}} }
func splitCommand(text string) (string, string) {
	s := strings.TrimSpace(text)
	i := strings.IndexAny(s, " \t\r\n")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}
