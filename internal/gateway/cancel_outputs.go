package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

// cancelPendingOutputs also closes a known reply card after its native render
// has finished. The cancellation and durable same-card status are atomic;
// already-issued requests can still arrive, so the notice makes no recall claim.
func (g *Gateway) cancelPendingOutputs(room core.Room, session string) error {
	var created []string
	err := g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT target,text,card,reply_to,thread_id,origins FROM outbox
			WHERE room=? AND session=? AND kind NOT IN ('status','file') AND target IS NOT NULL
			AND state IN ('pending','uncertain','blocked','sending','preparing') ORDER BY at DESC,id DESC`, room.Key, session)
		if err != nil {
			return err
		}
		type pending struct {
			target, text                 string
			card, reply, thread, origins sql.NullString
		}
		var pendingCards []pending
		seen := map[string]bool{}
		for rows.Next() {
			var output pending
			if err = rows.Scan(&output.target, &output.text, &output.card, &output.reply, &output.thread, &output.origins); err != nil {
				rows.Close()
				return err
			}
			if output.target != "" && !seen[output.target] {
				seen[output.target] = true
				pendingCards = append(pendingCards, output)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE outbox SET state='cancelled' WHERE room=? AND session=? AND kind!='status'
			AND state IN ('pending','uncertain','blocked','sending','preparing')`, room.Key, session); err != nil {
			return err
		}
		for _, output := range pendingCards {
			var already int
			if err = tx.QueryRow(`SELECT COUNT(*) FROM outbox WHERE room=? AND session=? AND kind='status' AND target=?
				AND state IN ('pending','uncertain','blocked','sending','preparing')`, room.Key, session, output.target).Scan(&already); err != nil {
				return err
			}
			if already != 0 {
				continue
			}
			var card core.Args
			_ = json.Unmarshal([]byte(output.card.String), &card)
			var who []core.Origin
			if output.origins.Valid && output.origins.String != "" && output.origins.String != "null" {
				// Do not discard an unreadable ownership binding just to update a
				// cosmetic status. The original output is still safely cancelled.
				if json.Unmarshal([]byte(output.origins.String), &who) != nil {
					continue
				}
			}
			card = cancelledOutputCard(card, output.text)
			id, err := g.saveOutput(tx, room, session, output.text, false, output.target, output.reply.String, output.thread.String, card, who, "", "", "status", "pending")
			if err != nil {
				return err
			}
			created = append(created, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !g.isClosing() {
		for _, id := range created {
			id := id
			g.task(func() { _, _ = g.deliverOutput(g.ctx, id, room, false) })
		}
	}
	return nil
}

func cancelledOutputCard(card core.Args, text string) core.Args {
	const title = "已停止 · 交付取消"
	const notice = "停止后不再安排交付；此前已发出的请求仍可能到达。"
	if card == nil {
		card = DisplayCard(title, text)
	}
	card = replaceQuickControls(card, nil)
	card["header"] = core.Args{"template": "grey", "title": core.Args{"tag": "plain_text", "content": title}}
	config := argsOf(card["config"])
	if config == nil {
		config = core.Args{}
	}
	config["streaming_mode"] = false
	delete(config, "streaming_config")
	config["summary"] = core.Args{"content": notice}
	card["config"] = config
	body := argsOf(card["body"])
	if body == nil {
		body = core.Args{}
	}
	elements, _ := body["elements"].([]any)
	for _, value := range elements {
		element := argsOf(value)
		if element["tag"] != "markdown" {
			continue
		}
		content := stringArg(element, "content")
		content = strings.ReplaceAll(content, "（完整答案另附文件。）", "（中断前预览已截断。）")
		content = strings.ReplaceAll(content, "（预览已截断，完成后附完整答案文件。）", "（中断前预览已截断。）")
		element["content"] = content
	}
	body["elements"] = append(elements, core.Args{"tag": "markdown", "element_id": "delivery_cancelled", "content": notice})
	card["body"] = body
	card = fitNoticeCard(card)
	if size, err := core.CardPayloadBytes(card); err != nil || size > core.CardPayloadLimit {
		// An oversized legacy/custom template may have large non-answer fields.
		// Use a bounded cancellation preview, never schedule a new export file.
		card = DisplayCard(title, text)
		card["header"] = core.Args{"template": "grey", "title": core.Args{"tag": "plain_text", "content": title}}
		card["config"] = core.Args{"streaming_mode": false, "summary": core.Args{"content": notice}}
		body = argsOf(card["body"])
		elements, _ = body["elements"].([]any)
		body["elements"] = append(elements, core.Args{"tag": "markdown", "element_id": "delivery_cancelled", "content": notice})
		card = fitNoticeCard(card)
	}
	return card
}
