package core

import "encoding/json"

// AnyGroupChatID marks the single groups[] entry used as the fallback for group
// chats without an explicit binding. It is not a real Feishu chat ID.
const AnyGroupChatID = "*"

// RoomFor resolves a group chat or DM to its room. An explicit group binding
// always wins; the wildcard binding applies only to otherwise unknown groups and
// never to DMs. The room key excludes member and mention policy so that policy
// changes do not rotate the native session.
func (c Config) RoomFor(chat, dm string) (Room, bool) {
	if dm == "" && chat == AnyGroupChatID {
		return Room{}, false
	}
	project := ""
	var members []string
	mentionOptional := false
	allowAllMembers := false
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
	}
	key, _ := json.Marshal([]any{c.TenantID, c.AppID, project, kind, chat, user})
	return Room{Key: string(key), ChatID: chat, ProjectID: project, DMUser: dm, Maintainers: p.Maintainers, Members: members, MentionOptional: mentionOptional, AllowAllMembers: allowAllMembers}, true
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
