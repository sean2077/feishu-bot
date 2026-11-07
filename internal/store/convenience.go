package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

func initializeConvenience(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS session_preferences(session TEXT PRIMARY KEY REFERENCES sessions(id), room TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', name_key TEXT NOT NULL DEFAULT '', profile TEXT NOT NULL DEFAULT '', requested_profile TEXT, revision TEXT NOT NULL DEFAULT '', native_title TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS session_name_room ON session_preferences(room,name_key) WHERE name_key!='';
CREATE TABLE IF NOT EXISTS card_controls(message TEXT PRIMARY KEY, room TEXT NOT NULL, session TEXT NOT NULL, revision TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS card_control_expiry ON card_controls(expires);
CREATE TABLE IF NOT EXISTS card_control_uses(message TEXT NOT NULL REFERENCES card_controls(message) ON DELETE CASCADE, revision TEXT NOT NULL, action TEXT NOT NULL, PRIMARY KEY(message,revision,action));
`)
	if err != nil {
		return err
	}
	return initializeTitleSync(tx)
}

type SessionPreferences struct {
	Name, Profile, Revision, NativeTitle string
	RequestedProfile                     *string
}

func (s *Store) Preferences(room, session string) (SessionPreferences, error) {
	var p SessionPreferences
	var requested sql.NullString
	err := s.DB.QueryRow(`SELECT coalesce(p.name,''),coalesce(p.profile,''),p.requested_profile,coalesce(p.revision,''),coalesce(p.native_title,'') FROM sessions s LEFT JOIN session_preferences p ON p.session=s.id AND p.room=s.room WHERE s.room=? AND s.id=?`, room, session).Scan(&p.Name, &p.Profile, &requested, &p.Revision, &p.NativeTitle)
	if requested.Valid {
		p.RequestedProfile = &requested.String
	}
	return p, err
}

// SetNativeTitle remembers the automatic Claude Code display title derived from
// the session's first admitted message. It is display metadata only: unlike
// RenameSession it never participates in chat-local name lookup.
func (s *Store) SetNativeTitle(room, session, title string) error {
	_, err := s.DB.Exec(`INSERT INTO session_preferences(session,room,native_title) VALUES(?,?,?) ON CONFLICT(session) DO UPDATE SET native_title=excluded.native_title WHERE session_preferences.room=excluded.room`, session, room, title)
	return err
}

func (s *Store) RequestProfile(room, session, profile string) error {
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var found string
		if err := tx.QueryRow("SELECT id FROM sessions WHERE room=? AND id=? AND state='open'", room, session).Scan(&found); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO session_preferences(session,room,requested_profile,revision) VALUES(?,?,?,?) ON CONFLICT(session) DO UPDATE SET requested_profile=excluded.requested_profile,revision=excluded.revision WHERE session_preferences.room=excluded.room`, session, room, profile, core.NewID())
		return err
	})
}

// ApplyProfile only commits the request whose process generation was stopped.
// A newer selection wins and will be applied by the next dispatch.
func (s *Store) ApplyProfile(room, session, revision string) (bool, error) {
	result, err := s.DB.Exec(`UPDATE session_preferences SET profile=requested_profile,requested_profile=NULL WHERE room=? AND session=? AND revision=? AND requested_profile IS NOT NULL AND EXISTS(SELECT 1 FROM sessions WHERE id=? AND room=? AND state='open')`, room, session, revision, session, room)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) RenameSession(room, session, name string) error {
	name = strings.TrimSpace(name)
	if name == "-" {
		name = ""
	} else if name == "" || utf8.RuneCountInString(name) > 80 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("名称须为 1–80 个字符，不能包含控制字符；用 - 清除名称")
	}
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var found string
		if err := tx.QueryRow("SELECT id FROM sessions WHERE room=? AND id=? AND state='open'", room, session).Scan(&found); err != nil {
			return err
		}
		key := strings.ToLower(name)
		if key != "" {
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM session_preferences WHERE room=? AND name_key=? AND session!=?`, room, key, session).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return errors.New("本聊天已有同名会话，请选择另一个名称")
			}
			if err := tx.QueryRow(`SELECT count(*) FROM sessions WHERE room=? AND id=? AND id!=?`, room, name, session).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return errors.New("名称不能使用其他会话的完整 ID")
			}
		}
		_, err := tx.Exec(`INSERT INTO session_preferences(session,room,name,name_key) VALUES(?,?,?,?) ON CONFLICT(session) DO UPDATE SET name=excluded.name,name_key=excluded.name_key WHERE session_preferences.room=excluded.room`, session, room, name, key)
		if err != nil {
			return err
		}
		_, err = prepareTitleSync(tx, room, session, "", true)
		return err
	})
}

// ResolveSession never searches another room. Exact IDs and names precede an
// unambiguous ID prefix; SQL wildcards are not treated as prefix characters.
func (s *Store) ResolveSession(room, query string) (string, error) {
	return s.ResolveSessionContext(context.Background(), room, query)
}

func (s *Store) ResolveSessionContext(ctx context.Context, room, query string) (string, error) {
	query = strings.TrimSpace(query)
	var id string
	err := s.DB.QueryRowContext(ctx, "SELECT id FROM sessions WHERE room=? AND id=?", room, query).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = s.DB.QueryRowContext(ctx, `SELECT s.id FROM sessions s JOIN session_preferences p ON p.session=s.id AND p.room=s.room WHERE s.room=? AND p.name_key=? AND p.name_key!=''`, room, strings.ToLower(query)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if len(query) < 6 || len(query) > 36 || strings.IndexFunc(query, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == '-') }) >= 0 {
		return "", errors.New("本聊天中没有此名称或会话 ID")
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT id FROM sessions WHERE room=? AND substr(id,1,?)=? ORDER BY id LIMIT 2", room, len(query), query)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 1 {
		return id, nil
	}
	if count > 1 {
		return "", errors.New("短 ID 对应多个会话，请提供更长的 ID")
	}
	return "", errors.New("本聊天中没有此名称或会话 ID")
}
