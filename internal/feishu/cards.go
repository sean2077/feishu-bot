package feishu

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/sean2077/feishu-bot/internal/core"
)

func canonicalJSON(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }
func cardTexts(value any) map[string]string {
	result := map[string]string{}
	var visit func(any, int)
	visit = func(v any, depth int) {
		if depth > 32 {
			return
		}
		switch item := v.(type) {
		case []any:
			for _, child := range item {
				visit(child, depth+1)
			}
		case map[string]any:
			id := stringValue(item, "element_id")
			if stringValue(item, "tag") == "markdown" && (id == "reply_answer" || id == "reply_thinking" || id == "reply_status") {
				if text, ok := item["content"].(string); ok {
					result[id] = text
				}
			}
			for _, child := range item {
				visit(child, depth+1)
			}
		}
	}
	visit(value, 0)
	return result
}
func normalizedCard(card core.Args) (core.Args, string, error) {
	encoded, err := canonicalJSON(card)
	if err != nil || len(encoded) > 1<<20 {
		return nil, "", errors.New("feishu: invalid or oversized card")
	}
	// Canonicalize []Args and other typed application slices into the same JSON
	// representation as snapshots restored from SQLite.
	var normalized core.Args
	if json.Unmarshal([]byte(encoded), &normalized) != nil {
		return nil, "", errors.New("feishu: invalid card")
	}
	return normalized, encoded, nil
}
func nativeCard(card core.Args) bool {
	config, ok := object(card["config"])
	if !ok || card["schema"] != "2.0" {
		return false
	}
	if _, ok = config["streaming_mode"].(bool); !ok {
		return false
	}
	_, ok = cardTexts(card)["reply_answer"]
	return ok
}
func cardStructure(v any) any {
	switch value := v.(type) {
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cardStructure(item)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		id := stringValue(value, "element_id")
		text := stringValue(value, "tag") == "markdown" && (id == "reply_answer" || id == "reply_thinking" || id == "reply_status")
		for key, item := range value {
			if key == "content" && text {
				continue
			}
			out[key] = cardStructure(item)
		}
		return out
	default:
		return v
	}
}
func (c *Client) Card(ctx context.Context, room core.Room, card core.Args, replyTo, threadID string) (string, error) {
	if _, err := c.validatedRoom(room); err != nil {
		// Permission requests in a non-maintainer DM are intentionally forwarded
		// to the project's maintainers. The gateway preserves the requesting
		// room key, while ChatID/DMUser identify the maintainer's open_id.
		// This exception is card-only and cannot authorize history, attachments,
		// normal messages, thread replies, or an arbitrary recipient.
		recipient, forwardErr := c.approvalRecipient(room)
		if forwardErr != nil || replyTo != "" || threadID != "" || nativeCard(card) {
			return "", err
		}
		_, snapshot, normalizeErr := normalizedCard(card)
		if normalizeErr != nil {
			return "", normalizeErr
		}
		var result struct {
			ID string `json:"message_id"`
		}
		if sendErr := c.api(ctx, http.MethodPost, "/im/v1/messages", url.Values{"receive_id_type": {"open_id"}}, core.Args{"receive_id": recipient, "msg_type": "interactive", "content": snapshot}, &result); sendErr != nil {
			return "", sendErr
		}
		if !validID(result.ID) {
			return "", &APIError{Operation: "approval delivery acknowledgement", Uncertain: true}
		}
		return result.ID, nil
	}
	if replyTo != "" {
		if err := c.checkReplyOrigin(ctx, replyTo, room); err != nil {
			return "", err
		}
	}
	if threadID != "" && replyTo == "" {
		return "", errors.New("feishu: thread reply requires an origin message")
	}
	normalized, snapshot, err := normalizedCard(card)
	if err != nil {
		return "", err
	}
	if c.db == nil || !nativeCard(normalized) {
		return c.plainCard(ctx, room, normalized, replyTo, threadID)
	}
	var result struct {
		ID string `json:"card_id"`
	}
	err = c.api(ctx, http.MethodPost, "/cardkit/v1/cards", nil, core.Args{"type": "card_json", "data": snapshot}, &result)
	if err != nil {
		if !permissionDenied(err) {
			return "", err
		}
		c.emit("card_stream_unavailable", core.Args{"reason": "cardkit_permission_denied", "fallback": "inline_card"})
		return c.plainCard(ctx, room, normalized, replyTo, threadID)
	}
	if !validID(result.ID) {
		return "", &APIError{Operation: "card creation acknowledgement", Uncertain: true}
	}
	// A failed send can mean that the message exists. Never create a replacement.
	id, err := c.sendContent(ctx, room, "interactive", core.Args{"type": "card", "data": core.Args{"card_id": result.ID}}, replyTo, threadID)
	if err != nil {
		return "", err
	}
	_, err = c.db.ExecContext(ctx, "INSERT INTO feishu_cards(message,card,room,sequence,snapshot) VALUES(?,?,?,0,?)", id, result.ID, room.Key, snapshot)
	if err != nil {
		return "", &APIError{Operation: "card delivery persistence", Uncertain: true}
	}
	return id, nil
}
func (c *Client) nextCardSequence(ctx context.Context, id string) (int64, error) {
	var seq int64
	// Commit before sending. A lost acknowledgement burns a sequence rather than
	// risking reuse of an already accepted write after a restart.
	err := c.db.QueryRowContext(ctx, "UPDATE feishu_cards SET sequence=sequence+1 WHERE message=? RETURNING sequence", id).Scan(&seq)
	if err != nil {
		return 0, errors.New("feishu: card sequence reservation failed")
	}
	return seq, nil
}
func (c *Client) Update(ctx context.Context, id string, card core.Args) error {
	if _, err := escapeID(id); err != nil {
		return err
	}
	unlock := c.locks.lock("card:" + id)
	defer unlock()
	normalized, snapshot, err := normalizedCard(card)
	if err != nil {
		return err
	}
	if c.db == nil {
		return c.plainUpdate(ctx, id, normalized)
	}
	var cardID, roomKey, previous string
	err = c.db.QueryRowContext(ctx, "SELECT card,room,snapshot FROM feishu_cards WHERE message=?", id).Scan(&cardID, &roomKey, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return c.plainUpdate(ctx, id, normalized)
	}
	if err != nil {
		return errors.New("feishu: card state is unavailable")
	}
	// Saved native cards retain their original room even if a callback supplies a
	// different target. Revoked configuration may no longer mutate that room.
	var parts []any
	if json.Unmarshal([]byte(roomKey), &parts) != nil || len(parts) != 6 {
		return errors.New("feishu: invalid card room")
	}
	chat, _ := parts[4].(string)
	if _, err = c.validatedRoom(core.Room{Key: roomKey, ChatID: chat}); err != nil {
		return err
	}
	if previous == snapshot {
		return nil
	}
	var old core.Args
	if json.Unmarshal([]byte(previous), &old) != nil {
		return errors.New("feishu: invalid saved card snapshot")
	}
	escaped, err := escapeID(cardID)
	if err != nil {
		return err
	}
	oldConfig, _ := object(old["config"])
	oldStructure, _ := canonicalJSON(cardStructure(old))
	newStructure, _ := canonicalJSON(cardStructure(normalized))
	if oldConfig["streaming_mode"] != true || !nativeCard(normalized) || oldStructure != newStructure {
		seq, e := c.nextCardSequence(ctx, id)
		if e != nil {
			return e
		}
		err = c.api(ctx, http.MethodPut, "/cardkit/v1/cards/"+escaped, nil, core.Args{"card": core.Args{"type": "card_json", "data": snapshot}, "sequence": seq, "uuid": "u_" + cardID + "_" + strconv.FormatInt(seq, 10)}, nil)
		if err != nil {
			return err
		}
	} else {
		before, after := cardTexts(old), cardTexts(normalized)
		keys := make([]string, 0, len(after))
		for key := range after {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if before[key] == after[key] {
				continue
			}
			seq, e := c.nextCardSequence(ctx, id)
			if e != nil {
				return e
			}
			escapedKey, _ := escapeID(key)
			err = c.api(ctx, http.MethodPut, "/cardkit/v1/cards/"+escaped+"/elements/"+escapedKey+"/content", nil, core.Args{"content": after[key], "sequence": seq, "uuid": "c_" + cardID + "_" + strconv.FormatInt(seq, 10)}, nil)
			if err != nil {
				return err
			}
		}
	}
	_, err = c.db.ExecContext(ctx, "UPDATE feishu_cards SET snapshot=? WHERE message=?", snapshot, id)
	if err != nil {
		return &APIError{Operation: "card snapshot persistence", Uncertain: true}
	}
	return nil
}
