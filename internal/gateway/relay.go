package gateway

import (
	"context"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

// adoptEndpoint is the live native CC inbox registered by the local terminal.
// It is kept in memory only: tokens never enter SQLite, logs or audit rows, and
// a gateway restart trusts only the next attach/notify request.
type adoptEndpoint struct {
	Socket     string
	Token      string
	ConfigPath string
	Native     string
}

func (g *Gateway) setAdoptEndpoint(room string, endpoint adoptEndpoint) {
	if room == "" {
		return
	}
	endpoint.Socket = strings.TrimSpace(endpoint.Socket)
	if endpoint.Socket == "" {
		g.clearAdoptEndpoint(room)
		return
	}
	g.mu.Lock()
	if g.adoptEndpoints == nil {
		g.adoptEndpoints = map[string]adoptEndpoint{}
	}
	g.adoptEndpoints[room] = endpoint
	g.mu.Unlock()
}

func (g *Gateway) endpointFor(room string) (adoptEndpoint, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	endpoint, ok := g.adoptEndpoints[room]
	return endpoint, ok
}

func (g *Gateway) clearAdoptEndpoint(room string) {
	g.mu.Lock()
	delete(g.adoptEndpoints, room)
	g.mu.Unlock()
}

// failForwarded marks unanswered forwarded messages uncertain when the local
// terminal no longer owns the session. They are never replayed.
func (g *Gateway) failForwarded(room core.Room, reason string) {
	g.clearAdoptEndpoint(room.Key)
	messages, err := g.store.FailForwarded(room.Key)
	if err != nil {
		if g.logger != nil {
			g.logger("topic_forward_fail_persistence_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		}
		return
	}
	if len(messages) == 0 {
		return
	}
	g.audit(room.Key, "topic_forward_failed", core.Args{"count": len(messages), "reason": reason})
	g.notifyAdoptOnce("forward-failed:"+room.Key, room, "本地终端已不再接管；之前已转递但未收到回复的消息不会重放，请重新发送。", nil)
}

// relayTopicMessage submits one owner DM topic message to the live local
// native inbox. A false return means the caller should use the existing queued
// holder path. The message is persisted before the external submission.
func (g *Gateway) relayTopicMessage(ctx context.Context, room core.Room, m core.Message) (bool, error) {
	if room.DMUser == "" || room.Thread == "" || m.ChatType != "p2p" || m.SenderID != room.DMUser {
		return false, nil
	}
	record, found, err := g.store.AdoptedTopicByRoom(room.Key)
	if err != nil || !found || record.State != store.AdoptedOpen || record.Session == "" {
		return false, nil
	}
	current, found, err := g.store.CurrentSession(room.Key)
	if err != nil || !found || current.ID != record.Session {
		return false, nil
	}
	if _, held := g.holderFor(room, record.Session); !held {
		return false, nil
	}
	endpoint, ok := g.endpointFor(room.Key)
	if !ok || endpoint.Native != record.Native {
		return false, nil
	}
	inserted, err := g.store.InsertForwarded(room.Key, m)
	if err != nil {
		// Fall back to the ordinary queue; storage will be retried there.
		return false, nil
	}
	if !inserted {
		return true, nil
	}
	text := relayWrapper(record, endpoint, m)
	if err = g.adoptSender(ctx, endpoint.Socket, endpoint.Token, text); err != nil {
		_, _ = g.store.MarkForwarded(room.Key, m.ID, "cancelled")
		g.audit(room.Key, "topic_forward_failed", core.Args{"count": 1, "reason": "inbox_submission", "message": m.ID})
		g.notifyAdoptOnce("relay-failed:"+room.Key, room, "转递失败，消息已按终端存活排队；终端退出后自动继续。", &m)
		return false, nil
	}
	g.acceptReceipt(room, record.Session, m)
	g.audit(room.Key, "topic_forwarded", core.Args{"message": m.ID, "native": record.Native})
	g.notifyAdoptOnce("relay:"+room.Key, room, "已提交给本机会话；回复会出现在本话题。", &m)
	return true, nil
}

func relayWrapper(record store.AdoptedTopic, endpoint adoptEndpoint, m core.Message) string {
	var builder strings.Builder
	builder.WriteString("[来自飞书私聊话题，网关已校验为 owner 本人]\n")
	builder.WriteString("消息ID: ")
	builder.WriteString(m.ID)
	builder.WriteString("\n内容:\n")
	builder.WriteString(m.Text)
	builder.WriteString("\n")
	if len(m.Resources) > 0 {
		builder.WriteString("[这条消息包含附件；本期转递未携带附件]\n")
	}
	builder.WriteString("回复方式: feishu-bot topic reply")
	if endpoint.ConfigPath != "" {
		builder.WriteString(" --config \"")
		builder.WriteString(strings.ReplaceAll(endpoint.ConfigPath, "\"", "\\\""))
		builder.WriteString("\"")
	}
	builder.WriteString(" --native ")
	builder.WriteString(record.Native)
	builder.WriteString(" --message-id ")
	builder.WriteString(m.ID)
	builder.WriteString(" --text-file -\n")
	return builder.String()
}
