package gateway

import (
	"encoding/json"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
	"unicode"
	"unicode/utf8"
)

func RoomFor(c core.Config, chat, dm string) (core.Room, bool) {
	return c.RoomFor(chat, dm)
}
func RoomFromKey(c core.Config, key string) (core.Room, bool) {
	return c.RoomForKey(key)
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
	return r.Authorizes(id)
}
func otherBot(m core.Message) bool { return m.SenderIsBot && !m.SenderIsSelf }
func Route(c core.Config, m core.Message) (core.Room, bool) {
	if m.SenderID == "" || m.ChatID == "" || m.ID == "" || (m.ChatType != "group" && m.ChatType != "p2p") {
		return core.Room{}, false
	}
	r, ok := c.MessageRoom(m)
	if !ok {
		return core.Room{}, false
	}
	if m.SenderIsBot {
		// Other bots are admitted only by an explicitly enabled group. This
		// flag is group-level authorization, not a maintainer or approval
		// grant, and never applies to a DM or the "*" fallback binding.
		if m.SenderIsSelf || m.ChatType != "group" || !r.AllowOtherBots {
			return core.Room{}, false
		}
		return r, true
	}
	return r, AuthorizedAuthor(r, m.SenderID)
}

// routeQueued re-authorizes an accepted input for roomKey under the current
// configuration. Feishu messages re-derive their room from the message itself.
// Card-composed supplements carry no Feishu topic fields; they stay bound to the
// room they were validated for, which must still resolve and still admit the
// author. Session and turn binding are checked separately by the callers.
func routeQueued(c core.Config, roomKey string, m core.Message) (core.Room, bool) {
	if m.InteractionMessageID == "" {
		r, ok := Route(c, m)
		return r, ok && r.Key == roomKey
	}
	if m.SenderIsBot || m.SenderID == "" || m.ID == "" {
		return core.Room{}, false
	}
	r, ok := c.RoomForKey(roomKey)
	kind := "group"
	if r.DMUser != "" {
		kind = "p2p"
	}
	if !ok || r.ChatID != m.ChatID || m.ChatType != kind {
		return core.Room{}, false
	}
	return r, AuthorizedAuthor(r, m.SenderID)
}
func Triggers(r core.Room, m core.Message) bool {
	if otherBot(m) {
		// Another bot must explicitly mention this bot; mentionOptional only
		// applies to human authors and must not turn bot chatter into a loop.
		return m.ChatType == "group" && m.Mentioned
	}
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
		return m.SenderIsSelf || (r.AllowOtherBots && m.ChatType == "group")
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

const nativeTitleRunes = 60

// cleanLine removes control characters and collapses whitespace for display.
func cleanLine(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

// nativeSessionTitle derives the native session-list title from the first
// admitted human message. The native process would otherwise name the session
// from the raw JSON input frame. Group messages keep their author; all other
// input stays as a single short line.
func nativeSessionTitle(m core.Message) string {
	text := cleanLine(m.Text)
	if text == "" {
		return ""
	}
	if m.ChatType == "group" {
		if sender := cleanLine(m.SenderName); sender != "" {
			text = sender + "：" + text
		}
	}
	if utf8.RuneCountInString(text) > nativeTitleRunes {
		text = cut(text, nativeTitleRunes-1) + "…"
	}
	return text
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
		o = append(o, core.Origin{ID: d.Message.ID, SenderID: d.Message.SenderID, SenderIsBot: d.Message.SenderIsBot})
	}
	return o
}
func origin(m core.Message) []core.Origin {
	return []core.Origin{{ID: m.ID, SenderID: m.SenderID, SenderIsBot: m.SenderIsBot}}
}
func splitCommand(text string) (string, string) {
	s := strings.TrimSpace(text)
	i := strings.IndexAny(s, " \t\r\n")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}
