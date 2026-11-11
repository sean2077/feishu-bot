package gateway

import (
	"context"
	"errors"

	"github.com/sean2077/feishu-bot/internal/core"
)

// historyRequest is the tool-call entry point. It keeps the existing topic and
// same-group chat paths untouched and adds the explicitly configured DM
// cross-chat path, sharing historyChatTarget/expandChatHistory with both.
func (g *Gateway) historyRequest(ctx context.Context, r core.Room, session string, input core.Args, requester *core.RequestContext) (core.Args, historyAccess, error) {
	current, ok := RoomFromKey(g.config, r.Key)
	if !ok || ctx.Err() != nil {
		return nil, historyAccess{}, errors.New("history scope is no longer available")
	}
	if _, has := input["chat"]; has {
		for key := range input {
			if !contains([]string{"chat", "limit", "startTime", "endTime", "threadId", "scope", "expandThreads"}, key) {
				return nil, historyAccess{}, errors.New("unsupported history parameter")
			}
		}
	}
	if raw, has := input["chat"]; has {
		value, ok := raw.(string)
		if !ok || len(value) > 200 {
			return nil, historyAccess{}, errors.New("invalid chat")
		}
		if _, hasCursor := input["cursor"]; hasCursor {
			return nil, historyAccess{}, errors.New("use the returned cursor alone for the next page")
		}
		target, access, err := g.crossChatHistoryTarget(current, session, requester, value)
		if err != nil {
			return nil, access, err
		}
		q, err := g.parseCrossChatHistoryQuery(target, input)
		if err != nil {
			return nil, access, err
		}
		return g.historyChatTarget(ctx, target, q)
	}
	if raw, has := input["cursor"]; has {
		if value, ok := raw.(string); ok && len(input) == 1 && len(value) <= 16000 {
			if q, err := g.openHistoryCursor(value, r.Key, session); err == nil && q.Chat != "" && q.Chat != current.ChatID {
				target, access, err := g.crossChatHistoryTarget(current, session, requester, q.Chat)
				if err != nil {
					return nil, access, err
				}
				q.Scope = historyScopeChat
				return g.historyChatTarget(ctx, target, q)
			}
		}
	}
	result, err := g.history(ctx, r, session, input)
	return result, historyAccess{}, err
}
