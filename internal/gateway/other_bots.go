package gateway

import (
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

const otherBotLimit = 2

const otherBotLimitNotice = "另一个机器人连续触发已达上限（2 轮），本条未触发。请人工确认后继续。"

type otherBotRound struct {
	count  int
	warned bool
}

func otherBotRoundKey(room core.Room, m core.Message) string {
	return room.Key + "\x00" + m.ThreadID + "\x00" + m.SenderID
}

// resetOtherBotRounds breaks the bot-to-bot streak when a human speaks in the
// same topic. Counters are in memory; a service restart is an explicit
// operator action and starts a new conversation window.
func (g *Gateway) resetOtherBotRounds(room core.Room, m core.Message) {
	prefix := room.Key + "\x00" + m.ThreadID + "\x00"
	g.otherBotMu.Lock()
	defer g.otherBotMu.Unlock()
	for key := range g.otherBotRounds {
		if strings.HasPrefix(key, prefix) {
			delete(g.otherBotRounds, key)
		}
	}
}

// otherBotTrigger admits at most two consecutive triggered turns per other bot
// in one topic. A non-triggering bot message is background and is not counted.
func (g *Gateway) otherBotTrigger(room core.Room, m core.Message) (allowed, warn bool) {
	key := otherBotRoundKey(room, m)
	g.otherBotMu.Lock()
	defer g.otherBotMu.Unlock()
	round := g.otherBotRounds[key]
	if round.count >= otherBotLimit {
		if !round.warned {
			round.warned = true
			g.otherBotRounds[key] = round
			return false, true
		}
		return false, false
	}
	round.count++
	g.otherBotRounds[key] = round
	return true, false
}

func (s *slot) claimMention() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor == "" || s.deliveries[s.anchor].UUID == "" {
		return false
	}
	if s.mentionAnchor != s.anchor {
		s.mentionAnchor = s.anchor
		s.mentionUsed = false
	}
	if s.mentionUsed {
		return false
	}
	s.mentionUsed = true
	return true
}

// mentionReply reports whether m is an other-bot reply to a mention this
// gateway sent. The outbox record makes the suppression survive restarts.
func (g *Gateway) mentionReply(room core.Room, m core.Message) bool {
	if !otherBot(m) || (m.ReplyTo == "" && m.ThreadID == "" && m.RootID == "") {
		return false
	}
	var n int
	err := g.store.DB.QueryRow(`SELECT count(*) FROM outbox WHERE room=? AND kind='mention' AND state IN ('done','pending','sending') AND (target=? OR (?<>'' AND thread_id=?) OR (?<>'' AND target=?))`, room.Key, m.ReplyTo, m.ThreadID, m.ThreadID, m.RootID, m.RootID).Scan(&n)
	return err == nil && n > 0
}
