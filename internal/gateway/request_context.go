package gateway

import (
	"sort"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	"`", "\\`",
	"*", "\\*",
	"_", "\\_",
	"~", "\\~",
	"[", "\\[",
	"]", "\\]",
	"(", "\\(",
	")", "\\)",
	"#", "\\#",
	">", "\\>",
	"<", "\\<",
	"-", "\\-",
	"+", "\\+",
	"=", "\\=",
	"|", "\\|",
	"{", "\\{",
	"}", "\\}",
	"!", "\\!",
	"&", "\\&",
)

// formattedRequester renders an untrusted display name safely for a markdown
// approval card and always includes the open ID.
func formattedRequester(context *core.RequestContext) string {
	if context == nil {
		return ""
	}
	name := cut(cleanLine(context.SenderName), 80)
	openID := strings.ReplaceAll(cleanLine(context.SenderOpenID), "`", "")
	switch {
	case openID == "":
		return markdownEscaper.Replace(name)
	case name == "":
		return "`" + openID + "`"
	default:
		return markdownEscaper.Replace(name) + "（`" + openID + "`）"
	}
}

// requestContext reads the slot's live deliveries and anchor. It returns nil
// when no human delivery is in progress; it never falls back to a previous
// message.
func (g *Gateway) requestContext(s *slot) *core.RequestContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return requestContextLocked(s)
}

func requestContextLocked(s *slot) *core.RequestContext {
	if len(s.deliveries) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.deliveries))
	seen := make(map[string]bool, len(s.deliveries))
	for _, id := range s.order {
		if _, ok := s.deliveries[id]; ok && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if len(ids) < len(s.deliveries) {
		// Defensive fallback: order normally tracks every live delivery.
		extra := make([]string, 0, len(s.deliveries)-len(ids))
		for id := range s.deliveries {
			if !seen[id] {
				extra = append(extra, id)
			}
		}
		sort.Strings(extra)
		ids = append(ids, extra...)
	}
	anchorID := s.anchor
	if _, ok := s.deliveries[anchorID]; !ok {
		anchorID = ids[0]
	}
	anchor := s.deliveries[anchorID]
	context := &core.RequestContext{
		ChatID:       anchor.Message.ChatID,
		ChatType:     anchor.Message.ChatType,
		ThreadID:     anchor.Message.ThreadID,
		MessageID:    anchor.Message.ID,
		SenderOpenID: anchor.Message.SenderID,
		SenderName:   anchor.Message.SenderName,
		Senders:      []string{},
		MessageIDs:   []string{},
	}
	senderSeen := map[string]bool{}
	for _, id := range ids {
		delivery := s.deliveries[id]
		sender := delivery.Message.SenderID
		if delivery.Message.SenderIsBot {
			context.HasBotSender = true
		}
		if sender != "" && !senderSeen[sender] {
			senderSeen[sender] = true
			context.Senders = append(context.Senders, sender)
		}
		if delivery.Message.ID != "" {
			context.MessageIDs = append(context.MessageIDs, delivery.Message.ID)
		}
	}
	return context
}
