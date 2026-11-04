package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type pendingEvent struct {
	message *core.Message
	action  *core.CardAction
}

func (c *Client) prepareEvent(payload []byte) (pendingEvent, error) {
	if len(payload) > 2<<20 {
		return pendingEvent{}, errors.New("feishu: event exceeds limit")
	}
	var envelope struct {
		Header struct {
			Type string `json:"event_type"`
		} `json:"header"`
		Type  string          `json:"type"`
		Event json.RawMessage `json:"event"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return pendingEvent{}, errors.New("feishu: malformed event")
	}
	kind := envelope.Header.Type
	if kind == "im.message.receive_v1" {
		var event messageEvent
		if json.Unmarshal(envelope.Event, &event) != nil {
			return pendingEvent{}, errors.New("feishu: malformed message event")
		}
		room, allowed := c.allowedHeader(event)
		if !allowed {
			return pendingEvent{}, nil
		}
		if event.Message.ChatType == "group" && c.config.Limits.BackgroundMessages == 0 && !botMentioned(event.Message, c.botOpenID()) {
			return pendingEvent{}, nil
		}
		message := normalizeMessage(event, c.botOpenID(), true)
		c.rememberOrigin(message.ID, room)
		return pendingEvent{message: &message}, nil
	}
	if kind == "card.action.trigger" || kind == "card.action.trigger_v1" || envelope.Type == "card.action.trigger" {
		var event struct {
			Operator struct {
				ID string `json:"open_id"`
			} `json:"operator"`
			Context struct {
				Chat    string `json:"open_chat_id"`
				Message string `json:"open_message_id"`
			} `json:"context"`
			Action struct {
				Value json.RawMessage `json:"value"`
				Form  core.Args       `json:"form_value"`
			} `json:"action"`
		}
		if json.Unmarshal(envelope.Event, &event) != nil {
			return pendingEvent{}, errors.New("feishu: malformed card event")
		}
		if !validID(event.Operator.ID) || !validID(event.Context.Chat) || !validID(event.Context.Message) {
			return pendingEvent{}, nil
		}
		room, ok := roomFor(c.config, event.Context.Chat, "")
		if ok {
			if !authorizedAuthor(room, event.Operator.ID) {
				return pendingEvent{}, nil
			}
		} else {
			if _, ok = roomFor(c.config, event.Context.Chat, event.Operator.ID); !ok && !c.configuredMaintainer(event.Operator.ID) {
				return pendingEvent{}, nil
			}
		}
		// A configured maintainer can receive an approval in their private chat
		// without granting that chat general bot-message or history access.
		// The gateway additionally verifies the durable request's exact room, user,
		// session and original message ID. Callback payload values are not authority.
		var value core.Args
		if json.Unmarshal(event.Action.Value, &value) != nil {
			var raw string
			if json.Unmarshal(event.Action.Value, &raw) != nil || json.Unmarshal([]byte(raw), &value) != nil {
				return pendingEvent{}, errors.New("feishu: invalid card action value")
			}
		}
		if value == nil {
			return pendingEvent{}, nil
		}
		action := core.CardAction{UserID: event.Operator.ID, ChatID: event.Context.Chat, MessageID: event.Context.Message, Value: value, FormValue: event.Action.Form}
		return pendingEvent{action: &action}, nil
	}
	return pendingEvent{}, nil
}
func (c *Client) runEvent(ctx context.Context, event pendingEvent) (reply any, err error) {
	defer func() {
		if recover() != nil {
			reply = nil
			err = errors.New("feishu: event handler failed")
		}
	}()
	if event.message != nil && c.onMessage != nil {
		err = c.onMessage(ctx, *event.message)
		if err != nil {
			c.emit("feishu_message_handler_error", formatSafeError(err))
		}
		return nil, err
	}
	if event.action != nil && c.onAction != nil {
		callbackCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		defer cancel()
		text, e := c.onAction(callbackCtx, *event.action)
		if e != nil {
			c.emit("feishu_card_handler_error", formatSafeError(e))
			return nil, e
		}
		if text == "" {
			return core.Args{}, nil
		}
		return core.Args{"toast": core.Args{"type": "info", "content": text}}, nil
	}
	return nil, nil
}

// HandleEvent exercises the identical boundary used by the long connection. It
// is useful for fixtures and webhooks hosted behind an independently verified
// signature/encryption layer; this method itself is not a public HTTP endpoint.
func (c *Client) HandleEvent(ctx context.Context, payload []byte) (any, error) {
	event, err := c.prepareEvent(payload)
	if err != nil {
		return nil, err
	}
	return c.runEvent(ctx, event)
}
