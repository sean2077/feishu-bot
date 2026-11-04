package feishu

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/sean2077/feishu-bot/internal/core"
)

func (c *Client) ownedReactions(ctx context.Context, id, emoji string) ([]string, error) {
	escaped, err := escapeID(id)
	if err != nil {
		return nil, err
	}
	result := []string{}
	cursor := ""
	botID := c.botOpenID()
	for page := 0; page < 10; page++ {
		query := url.Values{"reaction_type": {emoji}, "page_size": {"50"}}
		if cursor != "" {
			query.Set("page_token", cursor)
		}
		var data struct {
			Items []struct {
				ID       string `json:"reaction_id"`
				Operator struct {
					ID   string `json:"operator_id"`
					Type string `json:"operator_type"`
				} `json:"operator"`
			} `json:"items"`
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
		}
		if err = c.api(ctx, http.MethodGet, "/im/v1/messages/"+escaped+"/reactions", query, nil, &data); err != nil {
			return nil, err
		}
		for _, item := range data.Items {
			if validID(item.ID) && item.Operator.Type == "app" && (item.Operator.ID == c.config.AppID || botID != "" && item.Operator.ID == botID) {
				result = append(result, item.ID)
			}
		}
		if !data.HasMore {
			return result, nil
		}
		if data.PageToken == "" || data.PageToken == cursor || page == 9 {
			return nil, errors.New("feishu: reaction pagination is incomplete")
		}
		cursor = data.PageToken
	}
	return result, nil
}
func (c *Client) removeReaction(ctx context.Context, id, reactionID string) error {
	message, err := escapeID(id)
	if err != nil {
		return err
	}
	reaction, err := escapeID(reactionID)
	if err != nil {
		return err
	}
	return c.api(ctx, http.MethodDelete, "/im/v1/messages/"+message+"/reactions/"+reaction, nil, nil, nil)
}
func (c *Client) removeOwnedReactions(ctx context.Context, id, emoji string, required bool) error {
	ids, err := c.ownedReactions(ctx, id, emoji)
	if err != nil {
		return err
	}
	if required && len(ids) == 0 {
		return errors.New("feishu: reaction removal remains unconfirmed")
	}
	for _, reaction := range ids {
		if err = c.removeReaction(ctx, id, reaction); err != nil {
			return err
		}
	}
	return nil
}
func (c *Client) addReaction(ctx context.Context, id, emoji string) (string, error) {
	escaped, err := escapeID(id)
	if err != nil {
		return "", err
	}
	var data struct {
		ID string `json:"reaction_id"`
	}
	err = c.api(ctx, http.MethodPost, "/im/v1/messages/"+escaped+"/reactions", nil, core.Args{"reaction_type": core.Args{"emoji_type": emoji}}, &data)
	if err != nil {
		return "", err
	}
	if !validID(data.ID) {
		return "", &APIError{Operation: "reaction acknowledgement", Uncertain: true}
	}
	return data.ID, nil
}
func (c *Client) Receipt(ctx context.Context, id, state, previous string) error {
	if state == "" {
		state = "processing"
	}
	if state != "processing" && state != "done" && state != "clear" {
		return errors.New("feishu: invalid reaction state")
	}
	unlock := c.locks.lock("receipt:" + id)
	defer unlock()
	if state == "processing" {
		if previous == "attempt:processing" || previous == "processing" {
			ids, err := c.ownedReactions(ctx, id, "OnIt")
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				return errors.New("feishu: reaction creation remains unconfirmed")
			}
			c.rememberReaction(id, ids[0])
			return nil
		}
		reaction, err := c.addReaction(ctx, id, "OnIt")
		if err != nil {
			return err
		}
		c.rememberReaction(id, reaction)
		return nil
	}
	c.receiptMu.Lock()
	known := c.processingIDs[id]
	c.receiptMu.Unlock()
	if known != "" {
		if err := c.removeReaction(ctx, id, known); err != nil {
			return err
		}
		c.receiptMu.Lock()
		delete(c.processingIDs, id)
		c.receiptMu.Unlock()
	} else if previous != "done" && previous != "clear" {
		if err := c.removeOwnedReactions(ctx, id, "OnIt", previous == "attempt:processing" || previous == "processing"); err != nil {
			return err
		}
	}
	if state == "clear" && (previous == "done" || previous == "attempt:done") {
		return c.removeOwnedReactions(ctx, id, "Done", true)
	}
	if state == "done" {
		if previous == "attempt:done" || previous == "done" {
			ids, err := c.ownedReactions(ctx, id, "Done")
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				return errors.New("feishu: reaction creation remains unconfirmed")
			}
			return nil
		}
		_, err := c.addReaction(ctx, id, "Done")
		return err
	}
	return nil
}

func (c *Client) rememberReaction(id, reaction string) {
	c.receiptMu.Lock()
	defer c.receiptMu.Unlock()
	if len(c.processingIDs) >= 512 {
		for key := range c.processingIDs {
			delete(c.processingIDs, key)
			break
		}
	}
	c.processingIDs[id] = reaction
}
