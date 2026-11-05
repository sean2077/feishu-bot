package feishu

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

var _ core.CardRetainer = (*Client)(nil)
var _ core.CardRecovery = (*Client)(nil)

// Only these outbox references are read; this adapter does not change the
// gateway's delivery state or infer success from a card update.
type outboxColumns map[string]bool

func (c *Client) outboxColumns(ctx context.Context) (outboxColumns, error) {
	columns := outboxColumns{}
	rows, err := c.db.QueryContext(ctx, "PRAGMA table_info(outbox)")
	if err != nil { return nil, err }
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primary int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primary); err != nil { return nil, err }
		columns[name] = true
	}
	return columns, rows.Err()
}

type queryRowContext interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func outboxProtects(ctx context.Context, db queryRowContext, columns outboxColumns, message string, ids []string) (bool, error) {
	if !columns["state"] { return false, nil }
	var clauses []string
	var args []any
	for _, field := range []string{"target", "file_target"} {
		if columns[field] && message != "" { clauses = append(clauses, field+"=?"); args = append(args, message) }
	}
	for _, id := range ids {
		if columns["delivery_key"] { clauses = append(clauses, "delivery_key=?"); args = append(args, id) }
		if columns["id"] {
			clauses = append(clauses, "id=?", "'output:'||id=?", "'file:'||id=?")
			args = append(args, id, id, id)
		}
	}
	if len(clauses) == 0 { return false, nil }
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox WHERE state NOT IN ('done','cancelled') AND ("+strings.Join(clauses, " OR ")+")", args...).Scan(&count)
	return count > 0, err
}

func (c *Client) cardDeliveryIDs(ctx context.Context, message string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT DISTINCT id FROM feishu_deliveries WHERE message=? LIMIT 101", message)
	if err != nil { return nil, err }
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil { return nil, err }
		ids = append(ids, id)
	}
	if len(ids) > 100 { return nil, errors.New("feishu: too many card delivery references") }
	return ids, rows.Err()
}

// Retain removes old acknowledged terminal snapshots from SQLite. It does not
// remove messages in Feishu, vacuum the database, or make an RSS promise. Active
// renders, unfinished outbox entries and unresolved writes retain their records.
func (c *Client) Retain(ctx context.Context, before time.Time, protected []string) (int64, error) {
	if c.db == nil { return 0, nil }
	if c.isClosed() { return 0, errors.New("feishu: transport is closed") }
	unlock := c.locks.lock("card-maintenance")
	defer unlock()
	keep := map[string]bool{}
	for _, ref := range protected { if ref != "" { keep[ref] = true } }
	columns, err := c.outboxColumns(ctx)
	if err != nil { return 0, errors.New("feishu: outbox references are unavailable") }
	var removed int64
	lastAt, lastMessage := int64(0), ""
	for removed < 100 {
		rows, e := c.db.QueryContext(ctx, `SELECT c.message,l.terminal_at FROM feishu_cards c JOIN feishu_card_lifecycle l ON l.message=c.message
			WHERE l.terminal_at>0 AND l.terminal_at<? AND l.updated_at<? AND (l.terminal_at>? OR (l.terminal_at=? AND c.message>?))
			ORDER BY l.terminal_at,c.message LIMIT 100`, before.UnixMilli(), before.UnixMilli(), lastAt, lastAt, lastMessage)
		if e != nil { return removed, errors.New("feishu: card retention query failed") }
		type candidate struct { message string; at int64 }
		var messages []candidate
		for rows.Next() {
			var value candidate
			if e = rows.Scan(&value.message, &value.at); e != nil { rows.Close(); return removed, e }
			messages = append(messages, value)
		}
		e = rows.Err(); rows.Close()
		if e != nil { return removed, e }
		if len(messages) == 0 { break }
		for _, value := range messages {
			lastAt, lastMessage = value.at, value.message
			if keep[value.message] { continue }
			cardUnlock := c.locks.lock("card:"+value.message)
			n, e := c.pruneCard(ctx, value.message, before, keep, columns)
			cardUnlock()
			removed += n
			if e != nil { return removed, e }
			if removed >= 100 { break }
		}
	}
	if err = c.pruneDeliveries(ctx, before, keep, columns); err != nil { return removed, err }
	return removed, nil
}

func (c *Client) pruneCard(ctx context.Context, message string, before time.Time, keep map[string]bool, columns outboxColumns) (int64, error) {
	ids, err := c.cardDeliveryIDs(ctx, message)
	if err != nil { return 0, err }
	for _, id := range ids { if keep[id] { return 0, nil } }
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil { return 0, err }
	defer tx.Rollback()
	protected, err := outboxProtects(ctx, tx, columns, message, ids)
	if err != nil || protected { return 0, err }
	var unresolved int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feishu_deliveries d WHERE d.message=? AND
		EXISTS (SELECT 1 FROM feishu_deliveries pending WHERE pending.id=d.id AND pending.state NOT IN ('confirmed','rejected'))`, message).Scan(&unresolved)
	if err != nil || unresolved != 0 { return 0, err }
	result, err := tx.ExecContext(ctx, `DELETE FROM feishu_cards WHERE message=? AND EXISTS
		(SELECT 1 FROM feishu_card_lifecycle l WHERE l.message=feishu_cards.message AND l.terminal_at>0 AND l.terminal_at<? AND l.updated_at<?)`, message, before.UnixMilli(), before.UnixMilli())
	if err != nil { return 0, err }
	n, err := result.RowsAffected()
	if err != nil || n == 0 { return 0, err }
	if _, err = tx.ExecContext(ctx, "DELETE FROM feishu_card_lifecycle WHERE message=?", message); err != nil { return 0, err }
	// Delete the confirmed creation receipts with their terminal mapping. A
	// later startup must not interpret intentional retention as a crash between
	// acknowledgement and mapping persistence and reconstruct historical cards.
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "DELETE FROM feishu_deliveries WHERE id=?", id); err != nil { return 0, err }
	}
	if err = tx.Commit(); err != nil { return 0, err }
	return n, nil
}

func (c *Client) pruneDeliveries(ctx context.Context, before time.Time, keep map[string]bool, columns outboxColumns) error {
	lastAt, lastID, removed := int64(0), "", 0
	for removed < 100 {
		rows, err := c.db.QueryContext(ctx, `SELECT id,MAX(updated_at) FROM feishu_deliveries GROUP BY id HAVING MAX(updated_at)<?
			AND SUM(CASE WHEN state NOT IN ('confirmed','rejected') THEN 1 ELSE 0 END)=0
			AND (MAX(updated_at)>? OR (MAX(updated_at)=? AND id>?)) ORDER BY MAX(updated_at),id LIMIT 100`, before.UnixMilli(), lastAt, lastAt, lastID)
		if err != nil { return err }
		type candidate struct { id string; at int64 }
		var ids []candidate
		for rows.Next() { var value candidate; if err = rows.Scan(&value.id, &value.at); err != nil { rows.Close(); return err }; ids = append(ids, value) }
		err = rows.Err(); rows.Close()
		if err != nil { return err }
		if len(ids) == 0 { break }
		for _, value := range ids {
			lastAt, lastID = value.at, value.id
			if keep[value.id] { continue }
			// Lock every supported step before opening a transaction, in one stable
			// order. Never hold SQLite while waiting for an in-flight network call.
			createUnlock := c.locks.lock("delivery:"+value.id+":card-create")
			uploadUnlock := c.locks.lock("delivery:"+value.id+":file-upload")
			messageUnlock := c.locks.lock("delivery:"+value.id+":message")
			pruned, err := c.pruneDelivery(ctx, value.id, before, keep, columns)
			messageUnlock(); uploadUnlock(); createUnlock()
			if err != nil { return err }
			if pruned { removed++ }
			if removed >= 100 { break }
		}
	}
	return nil
}

func (c *Client) pruneDelivery(ctx context.Context, id string, before time.Time, keep map[string]bool, columns outboxColumns) (bool, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil { return false, err }
	defer tx.Rollback()
	var message string
	var unresolved, recent, cardExists int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(message),''),
		COALESCE(SUM(CASE WHEN state NOT IN ('confirmed','rejected') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN updated_at>=? THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN EXISTS(SELECT 1 FROM feishu_cards c WHERE c.message=feishu_deliveries.message) THEN 1 ELSE 0 END),0)
		FROM feishu_deliveries WHERE id=?`, before.UnixMilli(), id).Scan(&message, &unresolved, &recent, &cardExists)
	if err != nil { return false, err }
	if keep[message] || unresolved > 0 || recent > 0 || cardExists > 0 { return false, nil }
	protected, err := outboxProtects(ctx, tx, columns, message, []string{id})
	if err != nil || protected { return false, err }
	if _, err = tx.ExecContext(ctx, "DELETE FROM feishu_deliveries WHERE id=?", id); err != nil { return false, err }
	if err = tx.Commit(); err != nil { return false, err }
	return true, nil
}

// RecoverCards is a startup-only repair, before renders/model work can start.
// It can update an existing authorized card, but never create one or declare an
// interrupted turn Done. Pending outbox entries retain exclusive ownership.
func (c *Client) RecoverCards(ctx context.Context) (int64, error) {
	if c.db == nil { return 0, nil }
	if c.isClosed() { return 0, errors.New("feishu: transport is closed") }
	unlock := c.locks.lock("card-maintenance")
	defer unlock()
	if err := c.restoreCardMappings(ctx); err != nil { return 0, err }
	columns, err := c.outboxColumns(ctx)
	if err != nil { return 0, err }
	// Keyset pagination keeps startup memory bounded and does not retry a card
	// repeatedly within one recovery pass if the provider rejects its update.
	var recovered int64
	last := ""
	var failures []error
	for {
		rows, e := c.db.QueryContext(ctx, `SELECT c.message,c.room,c.snapshot,l.terminal_at FROM feishu_cards c
			LEFT JOIN feishu_card_lifecycle l ON l.message=c.message WHERE c.message>? AND COALESCE(l.terminal_at,0)=0 ORDER BY c.message LIMIT 100`, last)
		if e != nil { return recovered, e }
		type interruptedCard struct { message, room, snapshot string; terminal sql.NullInt64 }
		var cards []interruptedCard
		for rows.Next() { var card interruptedCard; if e = rows.Scan(&card.message, &card.room, &card.snapshot, &card.terminal); e != nil { rows.Close(); return recovered, e }; cards = append(cards, card) }
		e = rows.Err(); rows.Close()
		if e != nil { return recovered, e }
		if len(cards) == 0 { break }
		for _, card := range cards {
			last = card.message
			if ctx.Err() != nil { return recovered, ctx.Err() }
			ids, e := c.cardDeliveryIDs(ctx, card.message)
			if e != nil { return recovered, e }
			protected, e := outboxProtects(ctx, c.db, columns, card.message, ids)
			if e != nil { return recovered, e }
			if protected { continue }
			var roomParts []any
			if json.Unmarshal([]byte(card.room), &roomParts) != nil || len(roomParts) != 6 { continue }
			chat, _ := roomParts[4].(string)
			if _, e = c.validatedRoom(core.Room{Key: card.room, ChatID: chat}); e != nil { continue }
			var snapshot core.Args
			if json.Unmarshal([]byte(card.snapshot), &snapshot) != nil { failures = append(failures, errors.New("feishu: invalid interrupted card snapshot")); continue }
			config, _ := object(snapshot["config"])
			if !card.terminal.Valid && config["streaming_mode"] == false {
				// Pre-journal versions used streaming=false only for final native
				// replies. Adopt them without rewriting visible historical content;
				// their retention clock starts now, not at an invented old date.
				if e = c.touchCardState(ctx, card.message, false, true); e != nil { failures = append(failures, e) }
				continue
			}
			disableStreaming(snapshot)
			markCardInterrupted(snapshot)
			if e = c.Update(core.WithTerminalCard(ctx), card.message, snapshot); e != nil { failures = append(failures, e); continue }
			recovered++
		}
	}
	return recovered, errors.Join(failures...)
}

func markCardInterrupted(card core.Args) {
	const notice = "连接中断；本轮尚未确认完成。请查看 /status，必要时显式重试输出。"
	var answer core.Args
	statusFound := false
	var visit func(any)
	visit = func(value any) {
		switch item := value.(type) {
		case []any: for _, child := range item { visit(child) }
		case map[string]any:
			if item["element_id"] == "reply_status" && item["tag"] == "markdown" { item["content"] = notice; statusFound = true }
			if item["element_id"] == "reply_answer" && item["tag"] == "markdown" { answer = item }
			for _, child := range item { visit(child) }
		}
	}
	visit(card)
	header, ok := object(card["header"])
	if !ok { header = core.Args{}; card["header"] = header }
	header["template"] = "orange"
	header["title"] = core.Args{"tag": "plain_text", "content": "连接中断 · 尚未确认完成"}
	if !statusFound {
		if body, ok := object(card["body"]); ok {
			if elements, ok := body["elements"].([]any); ok {
				body["elements"] = append(elements, core.Args{"tag": "markdown", "element_id": "reply_status", "content": notice})
			}
		}
	}
	if config, ok := object(card["config"]); ok { config["summary"] = core.Args{"content": "连接中断，尚未确认完成"} }
	if answer == nil { return }
	// Make room for the notice using the actual nested JSON byte budget. Keep a
	// valid UTF-8 prefix; never silently send a payload that would be rejected.
	original := stringValue(answer, "content")
	if size, err := core.CardPayloadBytes(card); err == nil && size <= core.CardPayloadLimit { return }
	lo, hi := 0, min(len(original), core.CardPayloadLimit)
	for lo < hi {
		mid := lo+(hi-lo+1)/2
		prefix := mid
		for prefix > 0 && !utf8.ValidString(original[:prefix]) { prefix-- }
		answer["content"] = original[:prefix]+"…"
		if size, err := core.CardPayloadBytes(card); err == nil && size <= core.CardPayloadLimit { lo = mid } else { hi = mid-1 }
	}
	for lo > 0 && !utf8.ValidString(original[:lo]) { lo-- }
	answer["content"] = original[:lo]+"…"
}
