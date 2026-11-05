package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// A process can stop after both provider acknowledgements are journaled and
// before Card stores the message-to-entity mapping. Reconstruct that local
// mapping from the two immutable confirmed requests; never make another POST.
func (c *Client) restoreCardMappings(ctx context.Context) error {
	last := ""
	for {
		rows, err := c.db.QueryContext(ctx, `SELECT created.id,created.room,created.result,sent.result,created.payload,sent.payload
			FROM feishu_deliveries created JOIN feishu_deliveries sent ON sent.id=created.id AND sent.room=created.room
			WHERE created.step='card-create' AND sent.step='message' AND created.state='confirmed' AND sent.state='confirmed'
			AND created.id>? AND NOT EXISTS(SELECT 1 FROM feishu_cards c WHERE c.message=sent.result)
			ORDER BY created.id LIMIT 100`, last)
		if err != nil { return errors.New("feishu: card mapping recovery query failed") }
		type receipt struct { id, room, card, message, createPayload, sendPayload string }
		var receipts []receipt
		for rows.Next() {
			var r receipt
			if err = rows.Scan(&r.id, &r.room, &r.card, &r.message, &r.createPayload, &r.sendPayload); err != nil { rows.Close(); return err }
			receipts = append(receipts, r)
		}
		err = rows.Err(); rows.Close()
		if err != nil { return err }
		if len(receipts) == 0 { return nil }
		for _, r := range receipts {
			last = r.id
			if ctx.Err() != nil { return ctx.Err() }
			if !validID(r.card) || !validID(r.message) { continue }
			var parts []any
			if json.Unmarshal([]byte(r.room), &parts) != nil || len(parts) != 6 { continue }
			chat, _ := parts[4].(string)
			if _, err = c.validatedRoom(core.Room{Key: r.room, ChatID: chat}); err != nil { continue }
			var create, send core.Args
			if json.Unmarshal([]byte(r.createPayload), &create) != nil || json.Unmarshal([]byte(r.sendPayload), &send) != nil { continue }
			if stringValue(create, "method") != http.MethodPost || stringValue(create, "path") != "/cardkit/v1/cards" || stringValue(send, "method") != http.MethodPost { continue }
			createdBody, ok := object(create["body"])
			if !ok || stringValue(createdBody, "type") != "card_json" { continue }
			snapshot := stringValue(createdBody, "data")
			var card core.Args
			if json.Unmarshal([]byte(snapshot), &card) != nil || !nativeCard(card) { continue }
			sentBody, ok := object(send["body"])
			if !ok || stringValue(sentBody, "msg_type") != "interactive" { continue }
			var content core.Args
			if json.Unmarshal([]byte(stringValue(sentBody, "content")), &content) != nil || stringValue(content, "type") != "card" { continue }
			data, ok := object(content["data"])
			if !ok || stringValue(data, "card_id") != r.card { continue }
			// Restore mapping, explicit nonterminal lifecycle and association as
			// one transaction. A pending outbox is checked by RecoverCards next.
			tx, err := c.db.BeginTx(ctx, nil)
			if err != nil { return err }
			if _, err = tx.ExecContext(ctx, `INSERT INTO feishu_cards(message,card,room,sequence,snapshot) VALUES(?,?,?,0,?) ON CONFLICT(message) DO NOTHING`, r.message, r.card, r.room, snapshot); err == nil {
				_, err = tx.ExecContext(ctx, `INSERT INTO feishu_card_lifecycle(message,updated_at,terminal_at) VALUES(?,?,0) ON CONFLICT(message) DO NOTHING`, r.message, time.Now().UnixMilli())
			}
			if err == nil { _, err = tx.ExecContext(ctx, `UPDATE feishu_deliveries SET message=? WHERE id=? AND state='confirmed'`, r.message, r.id) }
			if err != nil { _ = tx.Rollback(); return err }
			if err = tx.Commit(); err != nil { return err }
		}
	}
}
