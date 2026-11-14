package core

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
)

// AnyGroupChatID marks the single groups[] entry used as the fallback for group
// chats without an explicit binding. It is not a real Feishu chat ID.
const AnyGroupChatID = "*"

// Group session scopes. The chat scope keeps one current session per group.
// The thread scope gives each Feishu topic (话题) its own room and session.
const (
	SessionScopeChat   = "chat"
	SessionScopeThread = "thread"
)

// ValidSessionScope reports whether v is an accepted groups[].sessionScope.
func ValidSessionScope(v string) bool {
	return v == "" || v == SessionScopeChat || v == SessionScopeThread
}

// HistoryChats returns the cross-chat history whitelist for one DM binding.
// The returned slice is a copy so callers cannot mutate the loaded config.
func (c Config) HistoryChats(dm string) []HistoryChat {
	for _, b := range c.DMs {
		if b.UserID == dm {
			return append([]HistoryChat(nil), b.HistoryChats...)
		}
	}
	return nil
}

// DM returns the configured private-chat binding for one user.
func (c Config) DM(user string) (DM, bool) {
	for _, d := range c.DMs {
		if d.UserID == user {
			return d, true
		}
	}
	return DM{}, false
}

// AdoptRoots returns the configured local-adoption roots for one DM binding.
// The returned slice is a copy so callers cannot mutate the loaded config.
func (c Config) AdoptRoots(dm string) []string {
	for _, b := range c.DMs {
		if b.UserID == dm {
			return append([]string(nil), b.AdoptRoots...)
		}
	}
	return nil
}

// AdoptEnabled reports whether the DM may adopt local sessions: the binding
// must opt in with at least one root and its project must inherit the live
// Claude configuration.
func (c Config) AdoptEnabled(dm string) bool {
	b, ok := c.DM(dm)
	if !ok || len(b.AdoptRoots) == 0 {
		return false
	}
	p, ok := c.Project(b.ProjectID)
	return ok && p.NativeConfig == "inherit"
}

// AdoptWorkAllowed reports whether work is inside one of the DM's configured
// adoption roots. Both sides must already be physical paths.
func (c Config) AdoptWorkAllowed(dm, work string) bool {
	if work == "" {
		return false
	}
	for _, root := range c.AdoptRoots(dm) {
		if Inside(root, work) {
			return true
		}
	}
	return false
}

// HistoryChatTarget resolves a configured name or chat ID within one DM.
// Names and IDs are validated unique together, so the match is unambiguous.
func (c Config) HistoryChatTarget(dm, key string) (HistoryChat, bool) {
	if key == "" {
		return HistoryChat{}, false
	}
	for _, h := range c.HistoryChats(dm) {
		if h.Name == key || h.ChatID == key {
			return h, true
		}
	}
	return HistoryChat{}, false
}

// HistoryChatRoom resolves a configured target to its explicitly bound group
// room. The wildcard group and DMs are never valid targets.
func (c Config) HistoryChatRoom(dm, key string) (Room, HistoryChat, bool) {
	h, ok := c.HistoryChatTarget(dm, key)
	if !ok || h.ChatID == AnyGroupChatID {
		return Room{}, HistoryChat{}, false
	}
	explicit := false
	for _, g := range c.Groups {
		if g.ChatID == h.ChatID {
			explicit = true
			break
		}
	}
	if !explicit {
		return Room{}, HistoryChat{}, false
	}
	r, ok := c.RoomFor(h.ChatID, "")
	return r, h, ok && r.DMUser == ""
}

// Group returns one explicitly bound group entry. The wildcard fallback is not
// a real chat and is never returned.
func (c Config) Group(chat string) (Group, bool) {
	if chat == "" || chat == AnyGroupChatID {
		return Group{}, false
	}
	for _, g := range c.Groups {
		if g.ChatID == chat {
			return g, true
		}
	}
	return Group{}, false
}

// RoomFor resolves a group chat or DM to its chat room. An explicit group
// binding always wins; the wildcard binding applies only to otherwise unknown
// groups and never to DMs. The room key excludes member, mention and session
// scope policy so that policy changes do not rotate the native session.
func (c Config) RoomFor(chat, dm string) (Room, bool) {
	if dm == "" && chat == AnyGroupChatID {
		return Room{}, false
	}
	project := ""
	var members []string
	mentionOptional := false
	allowAllMembers := false
	allowOtherBots := false
	var otherBots []OtherBot
	scope := ""
	topicHistory := ""
	if dm != "" {
		for _, b := range c.DMs {
			if b.UserID == dm {
				project = b.ProjectID
				break
			}
		}
	} else {
		var fallback *Group
		for i := range c.Groups {
			g := &c.Groups[i]
			if g.ChatID == chat {
				project = g.ProjectID
				members = g.Members
				mentionOptional = g.MentionOptional
				allowAllMembers = g.AllowAllMembers
				allowOtherBots = g.AllowOtherBots
				otherBots = g.OtherBots
				scope = g.SessionScope
				topicHistory = g.TopicHistory
				fallback = nil
				break
			}
			if g.ChatID == AnyGroupChatID && fallback == nil {
				fallback = g
			}
		}
		if fallback != nil {
			project = fallback.ProjectID
			members = fallback.Members
			mentionOptional = fallback.MentionOptional
			allowAllMembers = fallback.AllowAllMembers
			scope = fallback.SessionScope
			topicHistory = fallback.TopicHistory
		}
	}
	p, ok := c.Project(project)
	if !ok || project == "" {
		return Room{}, false
	}
	kind := "group"
	var user any
	if dm != "" {
		kind = "p2p"
		user = dm
		scope = ""
	}
	if scope == "" && dm == "" {
		scope = SessionScopeChat
	}
	key, _ := json.Marshal([]any{c.TenantID, c.AppID, project, kind, chat, user})
	return Room{Key: string(key), ChatKey: string(key), ChatID: chat, ProjectID: project, DMUser: dm, Maintainers: p.Maintainers, Members: members, MentionOptional: mentionOptional, AllowAllMembers: allowAllMembers, AllowOtherBots: allowOtherBots, OtherBots: otherBots, SessionScope: scope, TopicHistory: topicHistory}, true
}

// ThreadRoomFor resolves the room of one topic in a thread-scoped group. The
// key appends the topic root message ID to the unchanged chat room key fields,
// so chat-scoped keys and their sessions keep their existing namespace.
func (c Config) ThreadRoomFor(chat, root string) (Room, bool) {
	r, ok := c.RoomFor(chat, "")
	if !ok || r.SessionScope != SessionScopeThread || !ValidThreadRoot(root) {
		return Room{}, false
	}
	key, _ := json.Marshal([]any{c.TenantID, c.AppID, r.ProjectID, "group", chat, nil, root})
	r.Key = string(key)
	r.Thread = root
	for _, g := range c.Groups {
		if g.ChatID == chat {
			r.WorkspaceCWD = g.ThreadWorkspaces[root]
			break
		}
	}
	return r, true
}

// AdoptedTopicRoom resolves the room of one adopted DM topic. The key appends
// the registered root message ID to the unchanged DM room key fields. The
// workspace working directory is not part of the key; the gateway fills it
// from the durable adoption record after re-validating the configured roots.
func (c Config) AdoptedTopicRoom(chat, dm, root string) (Room, bool) {
	if dm == "" || !ValidThreadRoot(root) || !c.AdoptEnabled(dm) {
		return Room{}, false
	}
	base, ok := c.RoomFor(chat, dm)
	if !ok {
		return Room{}, false
	}
	key, _ := json.Marshal([]any{c.TenantID, c.AppID, base.ProjectID, "p2p", chat, dm, root})
	base.Key = string(key)
	// ChatKey stays the six-part DM room key: adopted topics share the DM
	// workspace, native HOME placement and chat-level inbox budget.
	base.Thread = root
	return base, true
}

// WorkingDirectory returns the explicit directory for this room, or an empty
// string for the managed, seeded workspace. Only an explicit topic binding
// overrides the project directory; neither value is taken from chat input.
func (r Room) WorkingDirectory(p Project) string {
	if r.Thread != "" && r.WorkspaceCWD != "" {
		return r.WorkspaceCWD
	}
	return p.CWD
}

// MessageRoom resolves the room that owns a received message. Thread-scoped
// groups map every top-level message to a new topic rooted at that message and
// every message inside a topic to the topic root; other chats use the chat room.
func (c Config) MessageRoom(m Message) (Room, bool) {
	dm := ""
	if m.ChatType == "p2p" {
		dm = m.SenderID
	} else if m.ChatType != "group" {
		return Room{}, false
	}
	r, ok := c.RoomFor(m.ChatID, dm)
	if !ok || r.SessionScope != SessionScopeThread {
		return r, ok
	}
	return c.ThreadRoomFor(m.ChatID, ThreadRoot(m))
}

// ThreadRoot returns the topic root of m. Only a real Feishu topic (thread_id)
// joins an existing root; a plain quoted reply also carries root_id but stays a
// top-level message and therefore roots its own topic.
func ThreadRoot(m Message) string {
	if m.ThreadID != "" && m.RootID != "" {
		return m.RootID
	}
	return m.ID
}

// ParseRoomKey extracts the chat, DM user and topic root from a room key. It
// checks only the key shape; RoomForKey also checks current configuration.
func ParseRoomKey(key string) (chat, dm, thread string, ok bool) {
	var parts []any
	if json.Unmarshal([]byte(key), &parts) != nil || len(parts) != 6 && len(parts) != 7 {
		return "", "", "", false
	}
	chat, ok = parts[4].(string)
	if !ok || chat == "" {
		return "", "", "", false
	}
	kind, _ := parts[3].(string)
	switch {
	case kind == "p2p" && len(parts) == 6:
		dm, ok = parts[5].(string)
		if !ok || dm == "" {
			return "", "", "", false
		}
	case kind == "p2p" && len(parts) == 7:
		dm, ok = parts[5].(string)
		if !ok || dm == "" {
			return "", "", "", false
		}
		thread, ok = parts[6].(string)
		if !ok || !ValidThreadRoot(thread) {
			return "", "", "", false
		}
	case kind == "group" && len(parts) == 6:
		if parts[5] != nil {
			return "", "", "", false
		}
	case kind == "group" && len(parts) == 7:
		thread, ok = parts[6].(string)
		if parts[5] != nil || !ok || !ValidThreadRoot(thread) {
			return "", "", "", false
		}
	default:
		return "", "", "", false
	}
	return chat, dm, thread, true
}

// RoomForKey resolves a stored room key under the current configuration. A
// group topic key is valid only while its group remains thread-scoped; a DM
// topic key is valid only while its DM still enables local adoption. The chat
// room key of the same chat stays valid so earlier chat sessions remain
// reachable.
func (c Config) RoomForKey(key string) (Room, bool) {
	chat, dm, thread, ok := ParseRoomKey(key)
	if !ok {
		return Room{}, false
	}
	var r Room
	if thread != "" {
		if dm != "" {
			r, ok = c.AdoptedTopicRoom(chat, dm, thread)
		} else {
			r, ok = c.ThreadRoomFor(chat, thread)
		}
	} else {
		r, ok = c.RoomFor(chat, dm)
	}
	return r, ok && r.Key == key
}

// WorkspaceKey places managed state and native HOME. Topics share this layout
// even when an explicit binding selects a different native working directory.
func (r Room) WorkspaceKey() string {
	if r.ChatKey != "" {
		return r.ChatKey
	}
	return r.Key
}

// AttachmentDir returns the incoming-attachment directory of r below its
// workspace root. Topic rooms share the chat workspace and native HOME but keep
// their own attachments, so a topic never discovers another topic's uploads.
func (r Room) AttachmentDir(root string) string {
	if r.Thread == "" {
		return filepath.Join(root, "inbound")
	}
	// A sibling tree, never nested under the chat's own inbound directory, so
	// no allowed directory contains another room's attachments.
	return filepath.Join(root, TopicInbound, Hash(r.Key)[:32])
}

// TopicInbound is the workspace-root directory holding per-topic attachments.
const TopicInbound = "topic-inbound"

// LegacyTopicAttachmentPath identifies the retired nested layout even if a
// migration was interrupted. A chat must never claim these topic uploads.
func LegacyTopicAttachmentPath(root, path string) bool {
	legacy := filepath.Join(root, "inbound", "topics")
	if runtime.GOOS == "darwin" {
		legacy, path = strings.ToLower(legacy), strings.ToLower(path)
	}
	return Inside(legacy, path)
}

// AttachmentRel returns the path of inbound relative to root when it is a
// valid AttachmentDir layout: "inbound" or "topic-inbound/<id>".
func AttachmentRel(root, inbound string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(inbound))
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 1 && parts[0] == "inbound" || len(parts) == 2 && parts[0] == TopicInbound && parts[1] != "" && parts[1] != "." && parts[1] != ".." {
		return rel, true
	}
	return "", false
}

// ValidThreadRoot accepts the platform message IDs used as topic roots.
func ValidThreadRoot(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

// Authorizes reports whether id may act as the author in this room.
func (r Room) Authorizes(id string) bool {
	if id == "" {
		return false
	}
	if r.DMUser != "" {
		return r.DMUser == id
	}
	if r.AllowAllMembers {
		return true
	}
	return containsID(r.Maintainers, id) || containsID(r.Members, id)
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
