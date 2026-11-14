package feishu

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/sean2077/feishu-bot/internal/core"
)

// SendUserCard sends one ordinary interactive card directly to a user's
// private chat by open_id. Unlike the approval-forwarding exception this is an
// explicit adoption building block: it returns both the message and the chat
// ID so the caller can create a topic rooted at that message. The call is
// journaled under the caller's delivery ID and is never retried blindly.
func (c *Client) SendUserCard(ctx context.Context, userID string, card core.Args) (messageID, chatID string, resultErr error) {
	defer func() { resultErr = localRejection("adoption card request validation", resultErr) }()
	if !validID(userID) {
		return "", "", errors.New("feishu: invalid adoption user")
	}
	_, snapshot, err := normalizedCard(card)
	if err != nil {
		return "", "", err
	}
	ctx = c.deliveryContext(ctx)
	body := core.Args{"receive_id": userID, "msg_type": "interactive", "content": snapshot}
	payload := core.Args{"method": http.MethodPost, "path": "/im/v1/messages", "query": url.Values{"receive_id_type": {"open_id"}}.Encode(), "body": body}
	var observedChat string
	messageID, err = c.deliveryStep(ctx, "adoption", "user-card", payload, true, func(attemptCtx context.Context, frozen core.Args, uuid string) (string, error) {
		requestBody, ok := object(frozen["body"])
		if !ok {
			return "", errors.New("feishu: invalid saved adoption card body")
		}
		requestBody["uuid"] = uuid
		values, err := url.ParseQuery(stringValue(frozen, "query"))
		if err != nil {
			return "", errors.New("feishu: invalid saved adoption query")
		}
		var result struct {
			MessageID string `json:"message_id"`
			ChatID    string `json:"chat_id"`
		}
		if err = c.api(attemptCtx, stringValue(frozen, "method"), stringValue(frozen, "path"), values, requestBody, &result); err != nil {
			return "", err
		}
		if !validID(result.MessageID) || !validID(result.ChatID) {
			return "", &APIError{Operation: "adoption card acknowledgement", Uncertain: true}
		}
		observedChat = result.ChatID
		return result.MessageID, nil
	})
	if err != nil {
		return "", "", err
	}
	if observedChat == "" {
		// A confirmed replay cannot recover the chat ID from the journal. Never
		// guess it from another chat: report an unconfirmed adoption instead.
		return messageID, "", &APIError{Operation: "adoption chat acknowledgement", Uncertain: true}
	}
	return messageID, observedChat, nil
}

var _ core.UserCardSender = (*Client)(nil)
