package store

import (
	"context"
	"strings"
)

// HistoryReplyTexts returns the delivered reply body for card message IDs in
// one room. Only terminal reply rows are eligible; file targets and undelivered
// or uncertain outputs are deliberately excluded.
func (s *Store) HistoryReplyTexts(ctx context.Context, room string, ids []string) (map[string]string, error) {
	return s.historyReplyTexts(ctx, "room=?", []any{room}, ids)
}

// HistoryReplyTextsChat is the same lookup across every topic room of one
// group chat. Room keys carry the chat ID at index 4; organization, project and
// DM identity remain fixed in the key, so another chat cannot match.
func (s *Store) HistoryReplyTextsChat(ctx context.Context, chat string, ids []string) (map[string]string, error) {
	if chat == "" {
		return map[string]string{}, nil
	}
	return s.historyReplyTexts(ctx, "json_extract(room,'$[4]')=?", []any{chat}, ids)
}

func (s *Store) historyReplyTexts(ctx context.Context, scope string, scopeArgs []any, ids []string) (map[string]string, error) {
	result := map[string]string{}
	if len(ids) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+len(scopeArgs))
	args = append(args, scopeArgs...)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT target,text FROM outbox
		WHERE `+scope+` AND state='done' AND kind='reply' AND text!='' AND target IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var target, text string
		if err = rows.Scan(&target, &text); err != nil {
			return nil, err
		}
		result[target] = text
	}
	return result, rows.Err()
}
