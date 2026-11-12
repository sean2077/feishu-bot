package gateway

import (
	"context"
	"database/sql"
)

// Recovery warnings and the state transition are one transaction. A second
// interruption cannot consume the evidence before its warning is durable.
func (g *Gateway) recoverInterrupted(ctx context.Context) error {
	return g.store.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT room,0,1 FROM inbox WHERE status IN ('sent','consumed')
UNION SELECT room,1,0 FROM requests WHERE state='pending'
UNION SELECT room,0,0 FROM action_runs WHERE state='running'`)
		if err != nil {
			return err
		}
		type roomRecovery struct{ request, interrupted bool }
		rooms := map[string]roomRecovery{}
		for rows.Next() {
			var room string
			var state roomRecovery
			if err = rows.Scan(&room, &state.request, &state.interrupted); err != nil {
				rows.Close()
				return err
			}
			current := rooms[room]
			current.request = current.request || state.request
			current.interrupted = current.interrupted || state.interrupted
			rooms[room] = current
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for key, state := range rooms {
			hadRequest, interrupted := state.request, state.interrupted
			r, ok := RoomFromKey(g.config, key)
			if !ok {
				continue
			}
			var session string
			err = tx.QueryRow("SELECT id FROM sessions WHERE room=? ORDER BY (state='open') DESC,created DESC LIMIT 1", key).Scan(&session)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			text := "上次运行中断。将继续原会话；未确认完成的输入与外部操作不会自动重放，请先核实结果。"
			if hadRequest {
				text += "\n\n上次未完成的审批／追问已失效，旧卡片不能继续授权操作。"
			}
			if hint := g.resumeHint(interrupted && !g.resumeBlocked(r, nil), r); hint != "" {
				text += " " + hint
			}
			if _, err = g.saveOutput(tx, r, session, text, false, "", "", "", DisplayCard("恢复提醒", text), nil, "", "", "recovery", ""); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE inbox SET status='uncertain' WHERE status IN ('sent','consumed');
UPDATE action_runs SET state='uncertain' WHERE state='running';
UPDATE outbox SET state=CASE WHEN kind IN ('file','artifact') AND file_state='done' AND file_target IS NOT NULL THEN 'pending' WHEN kind IN ('file','artifact') AND file_state='pending' THEN 'pending' WHEN target IS NULL OR file_state='sending' THEN 'uncertain' ELSE 'pending' END,
file_state=CASE WHEN file_state='sending' THEN 'uncertain' ELSE file_state END WHERE state IN ('sending','preparing');
UPDATE requests SET state='expired' WHERE state='pending';
UPDATE receipts SET desired='clear' WHERE desired='processing';`)
		return err
	})
}
