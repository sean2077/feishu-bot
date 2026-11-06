package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

// Activity is separate from native output and transport liveness. Replays and
// background output must never reorder the user's session catalogue.
func initializeSessionCatalog(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS session_activity(session TEXT PRIMARY KEY REFERENCES sessions(id),room TEXT NOT NULL,at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS session_activity_room ON session_activity(room,at DESC,session DESC);
CREATE INDEX IF NOT EXISTS sessions_room_created ON sessions(room,created DESC,id DESC);`)
	return err
}

// RecordSessionActivity belongs in the transaction admitting a new human input.
// Recovery, replay, native output and management reads must not call it.
func RecordSessionActivity(tx *sql.Tx, room, session string, at int64) error {
	if session == "" { return nil }
	result, err := tx.Exec(`INSERT INTO session_activity(session,room,at) SELECT id,room,? FROM sessions WHERE id=? AND room=? ON CONFLICT(session) DO UPDATE SET at=MAX(session_activity.at,excluded.at) WHERE session_activity.room=excluded.room`, at, session, room)
	if err != nil { return err }
	n,err:=result.RowsAffected()
	if err==nil&&n!=1 { return errors.New("session activity is outside the admitted room") }
	return err
}

type SessionEntry struct {
	core.Session
	Name string `json:"name,omitempty"`
	NativeTitle string `json:"nativeTitle,omitempty"`
	LastInputAt int64 `json:"lastInputAt"`
}

func (e SessionEntry) DisplayName() string {
	if e.Name != "" { return e.Name }
	if e.NativeTitle != "" { return e.NativeTitle }
	if len(e.ID) > 8 { return e.ID[:8] }
	return e.ID
}

type SessionQuery struct {
	Text string
	Before int64
	AfterAt int64
	AfterID string
	Limit int
}

// SearchSessions reads bounded pages of metadata in exactly one room. Text is
// literal (SQL wildcards have no special meaning); every whitespace-separated
// term must match. The caller signs the room, query and pagination boundary.
func (s *Store) SearchSessions(ctx context.Context, room string, q SessionQuery) ([]SessionEntry, bool, error) {
	terms := strings.Fields(q.Text)
	if room == "" || utf8.RuneCountInString(q.Text) > 128 || len(terms) > 8 || q.Before < 0 || q.AfterAt < 0 || len(q.AfterID) > 64 {
		return nil, false, errors.New("invalid session search")
	}
	limit := q.Limit
	if limit <= 0 { limit = 20 }
	if limit > 20 { return nil, false, errors.New("session page limit exceeds 20") }
	activity := "coalesce(a.at,s.created)"
	query := `SELECT s.id,s.room,s.cc,s.state,s.created,coalesce(p.profile,''),coalesce(p.name,''),coalesce(p.native_title,''),`+activity+` FROM sessions s LEFT JOIN session_preferences p ON p.session=s.id AND p.room=s.room LEFT JOIN session_activity a ON a.session=s.id AND a.room=s.room WHERE s.room=?`
	values := []any{room}
	if q.Before > 0 { query += " AND "+activity+"<=?"; values = append(values,q.Before) }
	for _, term := range terms {
		query += ` AND instr(lower(coalesce(p.name,'')||' '||coalesce(p.native_title,'')||' '||s.id),lower(?))>0`
		values = append(values,term)
	}
	if q.AfterID != "" {
		query += " AND ("+activity+"<? OR ("+activity+"=? AND s.id<?))"
		values = append(values,q.AfterAt,q.AfterAt,q.AfterID)
	}
	query += " ORDER BY "+activity+" DESC,s.id DESC LIMIT ?"
	values = append(values,limit+1)
	rows,err := s.DB.QueryContext(ctx,query,values...)
	if err != nil { return nil,false,err }
	defer rows.Close()
	items := []SessionEntry{}
	for rows.Next() {
		var item SessionEntry
		var cc sql.NullString
		if err = rows.Scan(&item.ID,&item.RoomKey,&cc,&item.State,&item.CreatedAt,&item.Profile,&item.Name,&item.NativeTitle,&item.LastInputAt); err != nil { return nil,false,err }
		item.CCID = cc.String
		items = append(items,item)
	}
	if err = rows.Err(); err != nil { return nil,false,err }
	more := len(items)>limit
	if more { items=items[:limit] }
	return items,more,nil
}
