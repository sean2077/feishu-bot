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
		if event.Message.ChatType == "group" && c.config.Limits.BackgroundMessages == 0 && !room.MentionOptional && !botMentioned(event.Message, c.botOpenID()) {
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
				Value  json.RawMessage `json:"value"`
				Option string          `json:"option"`
				Form   core.Args       `json:"form_value"`
			} `json:"action"`
		}
		if json.Unmarshal(envelope.Event, &event) != nil {
			c.emit("feishu_card_action_dropped", core.Args{"reason": "malformed"})
			return pendingEvent{}, errors.New("feishu: malformed card event")
		}
		eventType := kind
		if eventType == "" {
			eventType = envelope.Type
		}
		c.emit("feishu_card_action_received", core.Args{
			"type": eventType, "hasOperator": event.Operator.ID != "",
			"hasChat": event.Context.Chat != "", "hasMessage": event.Context.Message != "",
			"valueType": cardActionValueType(event.Action.Value),
		})
		if !validID(event.Operator.ID) || !validID(event.Context.Chat) || !validID(event.Context.Message) {
			c.emit("feishu_card_action_dropped", core.Args{"reason": "identity"})
			return pendingEvent{}, nil
		}
		room, ok := roomFor(c.config, event.Context.Chat, "")
		if ok {
			if !authorizedAuthor(room, event.Operator.ID) {
				c.emit("feishu_card_action_dropped", core.Args{"reason": "unauthorized"})
				return pendingEvent{}, nil
			}
		} else {
			if _, ok = roomFor(c.config, event.Context.Chat, event.Operator.ID); !ok && !c.configuredMaintainer(event.Operator.ID) {
				c.emit("feishu_card_action_dropped", core.Args{"reason": "unauthorized"})
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
				c.emit("feishu_card_action_dropped", core.Args{"reason": "invalid_value"})
				return pendingEvent{}, errors.New("feishu: invalid card action value")
			}
		}
		if value == nil {
			c.emit("feishu_card_action_dropped", core.Args{"reason": "missing_value"})
			return pendingEvent{}, nil
		}
		action := core.CardAction{UserID: event.Operator.ID, ChatID: event.Context.Chat, MessageID: event.Context.Message, Option: event.Action.Option, Value: value, FormValue: event.Action.Form}
		return pendingEvent{action: &action}, nil
	}
	return pendingEvent{}, nil
}

func cardActionValueType(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "missing"
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "invalid"
	}
	switch value.(type) {
	case map[string]any:
		return "object"
	case string:
		return "string"
	default:
		return "other"
	}
}

// cardCallbackBudget is measured from frame receipt, not from dequeue, so time
// spent waiting behind other callbacks counts against Feishu's deadline.
const cardCallbackBudget = 2500 * time.Millisecond

func (c *Client) runEvent(ctx context.Context, event pendingEvent, received time.Time) (reply any, err error) {
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
		callbackCtx, cancel := context.WithDeadline(ctx, received.Add(cardCallbackBudget))
		defer cancel()
		if callbackCtx.Err() != nil {
			// The client has already given up; acting now would commit an
			// input or decision the user was told did not happen.
			c.emit("feishu_card_action_dropped", core.Args{"reason": "deadline"})
			return nil, callbackCtx.Err()
		}
		reply, e := c.onAction(callbackCtx, *event.action)
		if e != nil {
			c.emit("feishu_card_handler_error", formatSafeError(e))
			return nil, e
		}
		if reply == nil {
			return core.Args{}, nil
		}
		return reply, nil
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
	return c.runEvent(ctx, event, time.Now())
}
