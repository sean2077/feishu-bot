package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"strings"
	"time"
)

type historyCursor struct {
	Room    string            `json:"room"`
	Session string            `json:"session"`
	Query   core.HistoryQuery `json:"query"`
	Expires int64             `json:"expires"`
}

func historyCardTextMissing(text string) bool {
	trimmed := strings.TrimSpace(text)
	return trimmed == "" || trimmed == core.HistoryCardPlaceholder
}

func (g *Gateway) history(ctx context.Context, r core.Room, session string, input core.Args) (core.Args, error) {
	current, ok := RoomFromKey(g.config, r.Key)
	if !ok || ctx.Err() != nil {
		return nil, errors.New("history scope is no longer available")
	}
	for k := range input {
		if !contains([]string{"limit", "startTime", "endTime", "cursor", "threadId", "scope", "expandThreads"}, k) {
			return nil, errors.New("unsupported history parameter")
		}
	}
	scope := historyScopeTopic
	if raw, has := input["scope"]; has {
		value, ok := raw.(string)
		if !ok || (value != historyScopeTopic && value != historyScopeChat) {
			return nil, errors.New("scope must be topic or chat")
		}
		scope = value
	}
	expandThreads := false
	if raw, has := input["expandThreads"]; has {
		value, ok := raw.(bool)
		if !ok {
			return nil, errors.New("expandThreads must be a boolean")
		}
		expandThreads = value
	}
	if raw, has := input["cursor"]; has {
		value, ok := raw.(string)
		if !ok || len(input) != 1 || len(value) > 16000 {
			return nil, errors.New("use the returned cursor alone for the next page")
		}
		cursorQuery, e := g.openHistoryCursor(value, r.Key, session)
		if e != nil {
			return nil, e
		}
		if cursorQuery.Scope == historyScopeChat {
			return g.historyChat(ctx, current, session, cursorQuery)
		}
	}
	if expandThreads && scope == historyScopeTopic {
		return nil, errors.New("expandThreads requires scope chat")
	}
	if scope == historyScopeChat {
		q, e := g.parseChatHistoryQuery(current, input)
		if e != nil {
			return nil, e
		}
		return g.historyChat(ctx, current, session, q)
	}
	q := core.HistoryQuery{Limit: 20, Scope: historyScopeTopic}
	if raw, has := input["cursor"]; has {
		s, ok := raw.(string)
		if !ok || len(input) != 1 || len(s) > 16000 {
			return nil, errors.New("use the returned cursor alone for the next page")
		}
		var e error
		q, e = g.openHistoryCursor(s, r.Key, session)
		if e != nil {
			return nil, e
		}
		if q.Scope == "" {
			q.Scope = historyScopeTopic
		}
	} else {
		if _, exists := input["limit"]; exists {
			v, ok := numberArg(input, "limit")
			if !ok || v < 1 || v > 50 {
				return nil, errors.New("limit must be 1..50")
			}
			q.Limit = int(v)
		}
		for _, key := range []string{"startTime", "endTime"} {
			if _, exists := input[key]; exists {
				v, ok := numberArg(input, key)
				if !ok {
					return nil, fmt.Errorf("%s must be a Unix timestamp in seconds", key)
				}
				if key == "startTime" {
					q.StartTime = v
				} else {
					q.EndTime = v
				}
			}
		}
		if value, exists := input["threadId"]; exists {
			s, ok := value.(string)
			if !ok || len(s) > 200 {
				return nil, errors.New("invalid thread ID")
			}
			q.ThreadID = s
		}
		if current.Thread != "" {
			// A topic session reads only its own topic. Other topics and the
			// chat timeline are reachable only through explicit quotes.
			topic, e := g.topicThread(ctx, current)
			if e != nil {
				return nil, e
			}
			if q.ThreadID != "" && q.ThreadID != topic {
				return nil, errors.New("this topic session can read only its own topic")
			}
			if topic == "" {
				return historyResult(core.Args{"messages": []core.Message{}, "nextCursor": nil, "hasMore": false, "note": "This topic has no earlier Feishu messages yet. Only the current topic is readable in this session."}), nil
			}
			q.ThreadID = topic
		}
		if q.ThreadID != "" {
			// Feishu does not filter thread containers by time.
			if q.StartTime > 0 || q.EndTime > 0 {
				return nil, errors.New("startTime and endTime are not supported for topic history; page with the cursor instead")
			}
		} else {
			if q.EndTime == 0 {
				q.EndTime = time.Now().Unix()
			}
			if q.StartTime > q.EndTime {
				return nil, errors.New("invalid history time range")
			}
		}
	}
	if current.Thread != "" && q.ThreadID == "" {
		return nil, errors.New("this topic session can read only its own topic")
	}
	if q.ThreadID != "" && current.Thread == "" {
		ok, e := g.store.HasContextThread(r.Key, q.ThreadID)
		if e != nil {
			return nil, e
		}
		if !ok {
			return nil, errors.New("thread must first be observed in this conversation")
		}
	}
	page, e := provider(ctx, func() (core.HistoryPage, error) { return g.transport.History(ctx, current, q) })
	if e != nil {
		return nil, e
	}
	latest, ok := RoomFromKey(g.config, r.Key)
	if ctx.Err() != nil || !ok {
		return nil, errors.New("history request cancelled")
	}
	selected := page.Messages[:min(q.Limit, len(page.Messages))]
	readable := make([]core.Message, 0, len(selected))
	ids := make([]string, 0, len(selected))
	for _, m := range selected {
		if !Readable(latest, m) {
			continue
		}
		if m.SenderIsBot && m.SenderIsSelf && historyCardTextMissing(m.Text) && m.ID != "" {
			ids = append(ids, m.ID)
		}
		readable = append(readable, m)
	}
	replyTexts, e := g.store.HistoryReplyTexts(ctx, r.Key, ids)
	if e != nil {
		// Backfill is additive context; a local lookup failure must not lose
		// the readable history page.
		g.audit(r.Key, "history_backfill_failed", core.Args{"error": core.Redact(e.Error(), g.secrets)})
		replyTexts = map[string]string{}
	}
	remaining := min(g.config.Limits.TextChars*2, 40000)
	messages := []core.Message{}
	for _, m := range readable {
		if text, found := replyTexts[m.ID]; found && m.SenderIsBot && m.SenderIsSelf && historyCardTextMissing(m.Text) {
			m.Text = text
		}
		m.Text = cut(m.Text, min(g.config.Limits.TextChars, remaining))
		remaining -= len([]rune(m.Text))
		if len(m.Resources) > 12 {
			m.Resources = m.Resources[:12]
		}
		messages = append(messages, m)
	}
	if e = g.store.RememberContext(r.Key, messages); e != nil {
		return nil, e
	}
	cursor := ""
	if page.NextPageToken != "" {
		q.PageToken = page.NextPageToken
		cursor = g.sealHistoryCursor(r.Key, session, q)
	}
	stampLocalTimes(messages)
	return historyResult(core.Args{"messages": messages, "nextCursor": nullString(cursor), "hasMore": cursor != "", "note": "Only authorized authors and this bot are returned. This is one API page, not the full history. Times are milliseconds in messages; query times use seconds. Use localTime for human-readable times."}), nil
}

// topicThread finds the Feishu thread ID of a topic room. Messages inside the
// topic carry it; a topic started by the bot's first reply is learned from the
// root or the bot's own replies and then remembered in this room.
func (g *Gateway) topicThread(ctx context.Context, r core.Room) (string, error) {
	id, e := g.store.TopicThread(r.Key, r.Thread)
	if e != nil || id != "" {
		return id, e
	}
	candidates := []string{r.Thread}
	outputs, e := g.store.RecentOutputs(r.Key, 2)
	if e != nil {
		return "", e
	}
	candidates = append(candidates, outputs...)
	for _, candidate := range candidates {
		m, e := provider(ctx, func() (*core.Message, error) { return g.transport.Fetch(ctx, candidate, r) })
		if e != nil {
			return "", e
		}
		if m != nil && m.ThreadID != "" && (m.ID == r.Thread || m.RootID == r.Thread) {
			if e = g.store.RememberContext(r.Key, []core.Message{*m}); e != nil {
				return "", e
			}
			return m.ThreadID, nil
		}
	}
	return "", nil
}
func (g *Gateway) sealHistoryCursor(room, session string, q core.HistoryQuery) string {
	b, _ := json.Marshal(historyCursor{room, session, q, time.Now().Add(15 * time.Minute).UnixMilli()})
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, g.historyKey[:])
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (g *Gateway) openHistoryCursor(value, room, session string) (core.HistoryQuery, error) {
	invalid := errors.New("history cursor expired or belongs to another conversation")
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return core.HistoryQuery{}, invalid
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return core.HistoryQuery{}, invalid
	}
	mac := hmac.New(sha256.New, g.historyKey[:])
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return core.HistoryQuery{}, invalid
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return core.HistoryQuery{}, invalid
	}
	var c historyCursor
	if e = json.Unmarshal(b, &c); e != nil || c.Room != room || c.Session != session || c.Expires < time.Now().UnixMilli() || c.Query.Limit < 1 || c.Query.Limit > 50 || (c.Query.Scope != "" && c.Query.Scope != historyScopeTopic && c.Query.Scope != historyScopeChat) || len([]rune(c.Query.Chat)) > 200 {
		return core.HistoryQuery{}, invalid
	}
	return c.Query, nil
}
