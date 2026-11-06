package gateway

import (
	"errors"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

func statusSummary(text string) string {
	summary := "**本聊天状态**"
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "状态：") {
			if state := strings.TrimSpace(strings.TrimPrefix(line, "状态：")); state != "" {
				summary += " · " + state
			}
			break
		}
	}
	return summary
}

func buildStatusCard(text string) core.Args {
	details := strings.TrimPrefix(text, "**本聊天状态**\n")
	return core.Args{
		"schema": "2.0",
		"config": core.Args{
			"width_mode":     "default",
			"update_multi":   true,
			"streaming_mode": false,
			"summary":        core.Args{"content": "本聊天状态"},
		},
		"body": core.Args{"elements": []any{
			core.Args{"tag": "markdown", "element_id": "reply_answer", "content": statusSummary(text)},
			core.Args{
				"tag":              "collapsible_panel",
				"expanded":         true,
				"vertical_spacing": "4px",
				"padding":          "4px 8px",
				"header":           core.Args{"title": core.Args{"tag": "plain_text", "content": "详细信息"}},
				"elements": []any{
					core.Args{"tag": "markdown", "element_id": "reply_status", "content": details},
				},
			},
		}},
	}
}

// A retryable or ambiguous card update must not create a second status card.
// Only a definitive rejection falls through to a replacement.
func statusRefreshUncertain(err error) bool {
	var failure core.DeliveryFailure
	if errors.As(err, &failure) {
		return failure.DeliveryUncertain() || failure.DeliveryRetryable()
	}
	return true
}

// refreshStatusCard keeps one status card per room. Repeat clicks update the
// same message instead of appending chat messages; a definitive update failure
// replaces the card only after the old message is known to be unusable.
func (g *Gateway) refreshStatusCard(r core.Room, actor string) {
	text, handled, err := g.management("/status", "", r, actor)
	if err != nil || !handled || text == "" {
		return
	}
	unlock, err := g.statusLocks.lockContext(g.ctx, r.Key)
	if err != nil {
		return
	}
	defer unlock()
	card := buildStatusCard(text)
	message, err := g.store.StatusCard(r.Key)
	if err != nil {
		g.audit(r.Key, "status_card_lookup_failed", core.Args{"error": err.Error()})
		return
	}
	if message != "" {
		if err := g.transport.Update(g.ctx, message, card); err == nil {
			return
		} else if statusRefreshUncertain(err) {
			g.audit(r.Key, "status_card_refresh_failed", core.Args{"error": err.Error()})
			return
		}
	}
	id, err := g.transport.Card(g.ctx, r, card, "", "")
	if err != nil {
		g.audit(r.Key, "status_card_create_failed", core.Args{"error": err.Error()})
		return
	}
	if err := g.store.SaveStatusCard(r.Key, id); err != nil {
		g.audit(r.Key, "status_card_persist_failed", core.Args{"error": err.Error()})
	}
}
