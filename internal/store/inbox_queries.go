package store

import (
	"context"
	"database/sql"
	"strings"
)

const pendingInputStates = "status IN ('queued','sent','consumed')"

type contextRowQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CountPendingChatTx uses the caller's transaction so admission and insertion
// reserve one shared chat budget, including simultaneous topic/card inputs.
func CountPendingChatTx(ctx context.Context, tx *sql.Tx, chatKey string) (int, error) {
	return countPendingChat(ctx, tx, chatKey)
}

func (s *Store) CountPendingChatContext(ctx context.Context, chatKey string) (int, error) {
	return countPendingChat(ctx, s.DB, chatKey)
}

func pendingChatQuery(chatKey string) (string, []any) {
	if !strings.HasSuffix(chatKey, "]") {
		return "SELECT count(*) FROM inbox WHERE room=? AND " + pendingInputStates, []any{chatKey}
	}
	// Topic keys extend the chat key's JSON array. This binary range does not
	// interpret underscores, percent signs or quoted characters as wildcards.
	prefix := chatKey[:len(chatKey)-1] + ","
	upper := chatKey[:len(chatKey)-1] + "-"
	return "SELECT count(*) FROM inbox WHERE (room=? OR (room>=? AND room<?)) AND " + pendingInputStates, []any{chatKey, prefix, upper}
}

func countPendingChat(ctx context.Context, q contextRowQuery, chatKey string) (int, error) {
	query, args := pendingChatQuery(chatKey)
	var n int
	err := q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

func (s *Store) SeenContext(ctx context.Context, room, id string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, "SELECT 1 FROM inbox WHERE room=? AND id=?", room, id).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return n == 1, err
}
