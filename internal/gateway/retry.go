package gateway

import (
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const retryConfirmWindow = 30 * time.Second

// retryConfirmed implements the overflow retry confirmation: because Feishu
// overflow options cannot carry a per-option confirm dialog, the first
// selection only arms a short-lived marker and the second confirms it.
func (g *Gateway) retryConfirmed(a core.CardAction, p quickControl) bool {
	now := time.Now()
	key := a.MessageID + "\x00" + p.Output + "\x00" + a.UserID
	g.retryConfirmMu.Lock()
	defer g.retryConfirmMu.Unlock()
	for k, expiry := range g.retryConfirm {
		if expiry < now.UnixMilli() {
			delete(g.retryConfirm, k)
		}
	}
	if expiry, ok := g.retryConfirm[key]; ok && expiry >= now.UnixMilli() {
		delete(g.retryConfirm, key)
		return true
	}
	g.retryConfirm[key] = now.Add(retryConfirmWindow).UnixMilli()
	return false
}
