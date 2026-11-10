package store

import (
	"context"
	"strings"
)

// HistoryReplyTexts returns the delivered reply body for card message IDs in
// one room. Only terminal reply rows are eligible; file targets and undelivered
// or uncertain outputs are deliberately excluded.
func (s *Store) HistoryReplyTexts(ctx context.Context, room string, ids []string) (map[string]string, error) {
	result := map[string]string{}
	if len(ids) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, room)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT target,text FROM outbox
		WHERE room=? AND state='done' AND kind='reply' AND text!='' AND target IN (`+strings.Join(placeholders, ",")+`)`, args...)
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
