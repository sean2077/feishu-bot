package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/store"
)

// adoptEndpoint is the live native CC inbox registered by the local terminal.
// It is kept in memory and, when complete, persisted as a per-native 0600 file
// so a gateway restart can restore relay while the same holder is alive.
// Tokens never enter SQLite, logs or audit rows.
type adoptEndpoint struct {
	Socket        string
	Token         string
	ConfigPath    string
	Native        string
	HolderPID     int
	HolderStarted int64
	// Restored marks an endpoint loaded after a restart. Its first submission
	// failure removes the persisted endpoint and falls back to the queue.
	Restored bool
}

func (g *Gateway) setAdoptEndpoint(room string, endpoint adoptEndpoint) {
	if room == "" {
		return
	}
	endpoint.Socket = strings.TrimSpace(endpoint.Socket)
	endpoint.Restored = false
	if endpoint.Socket == "" {
		g.removeAdoptEndpoint(room)
		return
	}
	previous, hadPrevious := g.endpointFor(room)
	g.mu.Lock()
	if g.adoptEndpoints == nil {
		g.adoptEndpoints = map[string]adoptEndpoint{}
	}
	g.adoptEndpoints[room] = endpoint
	g.mu.Unlock()
	if hadPrevious && previous.Native != "" && previous.Native != endpoint.Native {
		g.removeAdoptEndpointFile(previous.Native)
	}
	if strings.TrimSpace(endpoint.Token) == "" || !core.ValidThreadRoot(endpoint.Native) {
		g.removeAdoptEndpointFile(endpoint.Native)
		return
	}
	g.persistAdoptEndpoint(endpoint)
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

// removeAdoptEndpoint drops the in-memory endpoint and its persisted file. It
// is used when the holder is released, the session changes or a restored
// endpoint fails; a plain restart only clears memory and lets restore reload.
func (g *Gateway) removeAdoptEndpoint(room string) {
	endpoint, ok := g.endpointFor(room)
	g.clearAdoptEndpoint(room)
	if ok && endpoint.Native != "" {
		g.removeAdoptEndpointFile(endpoint.Native)
	}
}

// removeAdoptEndpointIfCurrent removes only the endpoint that failed, not a
// newer registration that replaced it while the submission was in flight. The
// map lock also covers the file removal so a concurrent persist cannot lose its
// file between the comparison and deletion.
func (g *Gateway) removeAdoptEndpointIfCurrent(room string, expected adoptEndpoint) {
	g.mu.Lock()
	current, ok := g.adoptEndpoints[room]
	if !ok || current != expected {
		g.mu.Unlock()
		return
	}
	delete(g.adoptEndpoints, room)
	if current.Native != "" {
		g.removeAdoptEndpointFile(current.Native)
	}
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
	text := g.relayWrapper(m, endpoint)
	outcome, err := g.adoptSender(ctx, endpoint.Socket, endpoint.Token, text)
	switch outcome {
	case native.InboxSubmitted:
		// A complete frame was submitted; delivery to the model is not
		// acknowledged and is not inferred here.
	case native.InboxUncertain:
		// The frame may have reached the terminal. Never replay it through the
		// ordinary queue; leave it as evidence and let the owner re-send.
		_, _ = g.store.MarkForwarded(room.Key, m.ID, "uncertain")
		g.audit(room.Key, "topic_forward_uncertain", core.Args{"message": m.ID, "native": record.Native})
		g.notifyAdoptOnce("relay-uncertain:"+room.Key, room, "转递结果未确认；消息不会自动重放，请检查本地终端或重新发送。", &m)
		return true, nil
	default:
		if endpoint.Restored {
			// A restored endpoint that definitely failed is stale; do not keep
			// trusting its persisted socket/token after a restart. Only remove
			// this exact registration: a concurrent notify may have refreshed it.
			g.removeAdoptEndpointIfCurrent(room.Key, endpoint)
		}
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

func (g *Gateway) relayWrapper(m core.Message, endpoint adoptEndpoint) string {
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
	builder.WriteString("回复方式: ")
	builder.WriteString(g.replyCommand())
	builder.WriteString(" topic reply --message-id ")
	builder.WriteString(m.ID)
	if flag := replyConfigFlag(endpoint.ConfigPath); flag != "" {
		builder.WriteString(flag)
	}
	builder.WriteString(" --text-file -\n")
	return builder.String()
}

// replyCommand returns the command word for the reply instruction. A bare
// feishu-bot is written only when PATH resolves to this gateway binary, so the
// local terminal can never silently run a different copy; otherwise the
// gateway's own absolute path is quoted.
func (g *Gateway) replyCommand() string {
	if g.selfExe != "" && g.exeLookup != nil {
		if found, err := g.exeLookup("feishu-bot"); err == nil && sameExecutable(found, g.selfExe) {
			return "feishu-bot"
		}
	}
	if g.selfExe == "" {
		return "feishu-bot"
	}
	return shellQuote(g.selfExe)
}

func sameExecutable(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(left, right)
}

// replyConfigFlag includes --config only when the endpoint was registered with
// a config outside the home default; the local terminal may carry no
// FEISHU_BOT_CONFIG, so an explicit path is always safe.
func replyConfigFlag(configPath string) string {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil &&
		core.SamePath(configPath, filepath.Join(home, ".feishu-bot", "config.json")) {
		return ""
	}
	return " --config " + shellQuote(configPath)
}

// shellQuote keeps a single command word literal for the local terminal's
// POSIX shell. Claude Code runs commands through Git Bash on Windows, where
// single quotes preserve backslashes in native paths.
func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`&|;()<>*?[]{}!#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
