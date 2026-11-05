package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"os"
	"path/filepath"
	"time"
)

type sqlExec interface {
	Exec(string, ...any) (sql.Result, error)
}
type outgoing struct {
	ID, Kind, Room, Session, Text, Target, ReplyTo, ThreadID, State, FileName, FilePath, FileTarget, FileState string
	Card                                                                                                       core.Args
	InitialCard                                                                                                core.Args
	DeliveryKey                                                                                                string
	RetryCount                                                                                                 int
	RetryAfter                                                                                                 int64
	Origins                                                                                                    []core.Origin
	Success                                                                                                    bool
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (g *Gateway) saveOutput(db sqlExec, r core.Room, session, text string, success bool, target, replyTo, thread string, card core.Args, who []core.Origin, filename, path, kind, state string) (string, error) {
	text = core.Redact(text, g.secrets)
	if card != nil {
		card = g.redactArgs(card)
	}
	id := core.NewID()
	fileState := "none"
	if path != "" {
		fileState = "pending"
	}
	if kind == "" {
		kind = "reply"
	}
	if state == "" {
		state = "pending"
	}
	var cardJSON any
	if card != nil {
		cardJSON = jsonText(card)
	}
	_, e := db.Exec(`INSERT INTO outbox(id,room,session,text,target,reply_to,thread_id,success,state,at,card,origins,file_name,file_path,file_state,kind) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, r.Key, session, text, nullString(target), nullString(replyTo), nullString(thread), success, state, time.Now().UnixMilli(), cardJSON, jsonText(who), nullString(filename), nullString(path), fileState, kind)
	return id, e
}
func (g *Gateway) output(id string) (outgoing, error) {
	var o outgoing
	var target, reply, thread, card, name, path, ft, raw, key, initial sql.NullString
	err := g.store.DB.QueryRow(`SELECT id,kind,room,session,text,target,reply_to,thread_id,success,state,card,origins,file_name,file_path,file_target,file_state,delivery_key,initial_card,retry_count,retry_after FROM outbox WHERE id=?`, id).Scan(&o.ID, &o.Kind, &o.Room, &o.Session, &o.Text, &target, &reply, &thread, &o.Success, &o.State, &card, &raw, &name, &path, &ft, &o.FileState, &key, &initial, &o.RetryCount, &o.RetryAfter)
	if err != nil {
		return o, err
	}
	o.Target = target.String
	o.ReplyTo = reply.String
	o.ThreadID = thread.String
	o.FileName = name.String
	o.FilePath = path.String
	o.FileTarget = ft.String
	o.DeliveryKey = key.String
	if o.DeliveryKey == "" { o.DeliveryKey = "output:" + o.ID }
	if initial.Valid && initial.String != "" {
		if err = json.Unmarshal([]byte(initial.String), &o.InitialCard); err != nil { return o, err }
	}
	if card.Valid && card.String != "" {
		if err = json.Unmarshal([]byte(card.String), &o.Card); err != nil {
			return o, err
		}
	}
	if raw.Valid && raw.String != "" && raw.String != "null" {
		if err = json.Unmarshal([]byte(raw.String), &o.Origins); err != nil {
			return o, err
		}
	}
	return o, nil
}
func (g *Gateway) outputAllowed(o outgoing, r core.Room) bool {
	current, ok := RoomFromKey(g.config, r.Key)
	if !ok {
		return false
	}
	if len(o.Origins) > 0 {
		for _, v := range o.Origins {
			if !AuthorizedAuthor(current, v.SenderID) {
				return false
			}
		}
		return true
	}
	if o.ReplyTo == "" {
		return true
	}
	m, err := g.store.ContextMessage(r.Key, o.ReplyTo)
	return err == nil && m != nil && Readable(current, *m)
}
func (g *Gateway) permittedOutput(o outgoing, r core.Room) bool {
	if g.outputAllowed(o, r) {
		return true
	}
	_, _ = g.store.DB.Exec(`UPDATE outbox SET state='cancelled',text='',card=NULL WHERE id=?`, o.ID)
	g.settleReceipts(r.Key, o.Session, o.Origins, false)
	return false
}
func (g *Gateway) outputSending(id string, r core.Room) bool {
	o, e := g.output(id)
	return e == nil && o.State == "sending" && g.permittedOutput(o, r)
}
func (g *Gateway) unfinishedFiles(room, session string) int {
	var n int
	_ = g.store.DB.QueryRow(`SELECT count(*) FROM outbox WHERE room=? AND session=? AND kind='file' AND state NOT IN ('done','cancelled')`, room, session).Scan(&n)
	return n
}
func (g *Gateway) deliverOutput(ctx context.Context, id string, r core.Room, explicit bool) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	g.outMu.Lock()
	if g.outActive[id] {
		g.outMu.Unlock()
		return false, nil
	}
	g.outActive[id] = true
	g.outMu.Unlock()
	defer func() { g.outMu.Lock(); delete(g.outActive, id); g.outMu.Unlock() }()
	o, err := g.output(id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if o.Room != r.Key || o.State == "done" || o.State == "cancelled" || !g.permittedOutput(o, r) {
		return false, nil
	}
	if (o.State != "pending" && o.State != "uncertain" && o.State != "blocked") || ((o.State == "uncertain" || o.State == "blocked") && !explicit) || (o.FileState == "uncertain" && !explicit) {
		return false, nil
	}
	if !explicit && o.RetryAfter > time.Now().UnixMilli() { return false, nil }
	ctx = core.WithDeliveryID(ctx, o.DeliveryKey)
	if explicit { ctx = core.WithExplicitRetry(ctx) }
	if err = g.budgetOutput(&o); err != nil { return false, err }
	result, err := g.store.DB.Exec(`UPDATE outbox SET state='sending',delivery_error=NULL WHERE id=? AND state=?`, id, o.State)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return false, nil
	}
	target := o.Target
	phase := "card"
	knownFile := o.FileState == "done" && o.FileTarget != ""
	if o.Kind == "file" {
		phase = "file_prepare"
	}
	perform := func() error {
		if o.Kind != "file" {
			card := g.outputCard(o, r, (o.FileName != "" && !knownFile) || o.Kind == "reply" && o.Success && g.unfinishedFiles(r.Key, o.Session) > 0)
			if target == "" {
				if o.InitialCard == nil {
					o.InitialCard = card
					if _, e := g.store.DB.Exec("UPDATE outbox SET initial_card=? WHERE id=? AND state='sending'", jsonText(card), id); e != nil { return e }
				}
				target, err = provider(ctx, func() (string, error) { return g.transport.Card(ctx, r, o.InitialCard, o.ReplyTo, o.ThreadID) })
				if err != nil { return err }
				if target != "" {
					if _, err = g.store.DB.Exec("UPDATE outbox SET target=? WHERE id=? AND state='sending'", target, id); err != nil { return err }
				}
			}
			if !g.outputSending(id, r) { return errCancelledDelivery }
			updateCtx := ctx
			if o.FileName == "" || knownFile { updateCtx = core.WithTerminalCard(ctx) }
			if target != "" {
				err = providerVoid(updateCtx, func() error { return g.transport.Update(updateCtx, target, card) })
				if err != nil { return err }
			}
			if target == "" {
				return errors.New("provider did not return message ID")
			}
			res, e := g.store.DB.Exec(`UPDATE outbox SET target=? WHERE id=? AND state='sending'`, target, id)
			if e != nil {
				return e
			}
			changed, _ := res.RowsAffected()
			if changed == 0 || !g.outputSending(id, r) {
				return errCancelledDelivery
			}
			if e = g.store.RecordOutput(target, r.Key, o.Session); e != nil {
				return e
			}
			if e = g.bindQuickControls(target, card); e != nil { return e }
		}
		if (o.Kind == "file" || o.FileName != "") && !knownFile {
			phase = "file_prepare"
			if o.FilePath == "" || o.FileName == "" {
				return errors.New("missing export snapshot")
			}
			if o.Kind != "file" {
				if e := os.MkdirAll(filepath.Dir(o.FilePath), 0700); e != nil {
					return e
				}
				if e := os.WriteFile(o.FilePath, []byte(o.Text), 0600); e != nil {
					return e
				}
			} else if _, e := os.Stat(o.FilePath); e != nil {
				return e
			}
			if !g.outputSending(id, r) {
				return errCancelledDelivery
			}
			res, e := g.store.DB.Exec(`UPDATE outbox SET file_state='sending' WHERE id=? AND state='sending'`, id)
			if e != nil {
				return e
			}
			changed, e := res.RowsAffected()
			if e != nil {
				return e
			}
			if changed != 1 || !g.outputSending(id, r) {
				return errCancelledDelivery
			}
			phase = "file_send"
			fileCtx := core.WithDeliveryID(ctx, "file:" + o.ID)
			ft, e := provider(fileCtx, func() (string, error) { return g.transport.File(fileCtx, r, o.FilePath, o.FileName) })
			if e != nil {
				return e
			}
			if ft == "" {
				return errors.New("provider did not return file message ID")
			}
			knownFile = true
			o.FileTarget = ft
			res, e = g.store.DB.Exec(`UPDATE outbox SET file_target=?,file_state='done' WHERE id=? AND state='sending'`, ft, id)
			if e != nil {
				return e
			}
			changed, _ = res.RowsAffected()
			if changed == 0 || !g.outputSending(id, r) {
				return errCancelledDelivery
			}
			if e = g.store.RecordOutput(ft, r.Key, o.Session); e != nil {
				return e
			}
		} else if o.Kind == "file" {
			if !knownFile {
				return errors.New("missing file receipt")
			}
			if e := g.store.RecordOutput(o.FileTarget, r.Key, o.Session); e != nil {
				return e
			}
		}
		if !g.outputSending(id, r) {
			return errCancelledDelivery
		}
		if o.Kind == "reply" && o.Success && g.unfinishedFiles(r.Key, o.Session) > 0 {
			_, e := g.store.DB.Exec(`UPDATE outbox SET state='pending',delivery_error=? WHERE id=? AND state='sending'`, "Attachment delivery remains pending.", id)
			if e != nil {
				return e
			}
			g.pendingOutputNotice(ctx, o, r, target, "主动发送的文件尚未确认送达；本轮未标记完成。可用 /outputs 查看记录。")
			return errDeferredDelivery
		}
		if o.Kind != "file" && o.FileName != "" && knownFile {
			phase = "card_finalize"
			card := g.outputCard(o, r, false)
			finalCtx := core.WithTerminalCard(ctx)
			if e := providerVoid(finalCtx, func() error { return g.transport.Update(finalCtx, target, card) }); e != nil { return e }
			if e := g.bindQuickControls(target, card); e != nil { return e }
		}
		res, e := g.store.DB.Exec(`UPDATE outbox SET state='done',delivery_error=NULL,retry_count=0,retry_after=0 WHERE id=? AND state='sending'`, id)
		if e != nil {
			return e
		}
		changed, _ := res.RowsAffected()
		if changed == 0 {
			return errCancelledDelivery
		}
		if o.Kind == "reply" {
			g.settleReceipts(r.Key, o.Session, o.Origins, o.Success)
		}
		return nil
	}
	err = perform()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errCancelledDelivery) || errors.Is(err, errDeferredDelivery) {
		return false, nil
	}
	state := "pending"
	reason := "Reply target saved; local delivery completion is pending."
	var failure core.DeliveryFailure
	classified := errors.As(err, &failure)
	uncertain := !classified || failure.DeliveryUncertain()
	if phase == "card" && target == "" && uncertain {
		state = "uncertain"
		reason = "Reply card delivery is unconfirmed; check whether it arrived before explicitly retrying."
	} else if phase == "file_send" && !knownFile && uncertain {
		state = "uncertain"
		reason = "File delivery unconfirmed; explicit retry may duplicate a previously accepted message."
	} else if phase == "file_prepare" {
		reason = "Cannot read or prepare the file; check the data directory permissions and capacity."
	}
	if classified && !failure.DeliveryUncertain() {
		reason = core.Redact(err.Error(), g.secrets)
		if !failure.DeliveryRetryable() { state = "blocked" }
	}
	attempt := o.RetryCount + 1
	if explicit { attempt = 1 }
	if state == "pending" && attempt >= 6 {
		state = "blocked"
		reason += " Automatic retry budget exhausted; resolve the cause and use /retry-output."
	}
	delay := time.Duration(1<<min(attempt, 8)) * time.Second
	if classified && failure.DeliveryRetryAfter() > delay { delay = failure.DeliveryRetryAfter() }
	fileState := ""
	if phase == "file_send" && !knownFile {
		fileState = "pending"
		if state == "uncertain" { fileState = "uncertain" }
	}
	_, dbErr := g.store.DB.Exec(`UPDATE outbox SET state=?,target=COALESCE(?,target),file_state=CASE WHEN ?!='' THEN ? ELSE file_state END,delivery_error=?,retry_count=?,retry_after=? WHERE id=? AND state='sending'`, state, nullString(target), fileState, fileState, reason, attempt, time.Now().Add(delay).UnixMilli(), id)
	g.audit(r.Key, "output_delivery_failed", core.Args{"id": id, "phase": phase, "kind": o.Kind, "knownTarget": target != "", "knownFile": knownFile})
	if ctx.Err() == nil && target != "" && phase != "card" && !knownFile {
		g.pendingOutputNotice(ctx, o, r, target, "完整文件尚未确认送达；未标记完成。可用 /outputs 查看状态；明确补发可能重复。")
	}
	if dbErr != nil {
		return false, dbErr
	}
	return false, err
}

var errCancelledDelivery = errors.New("output cancelled")
var errDeferredDelivery = errors.New("output waiting for attachments")

func (g *Gateway) pendingOutputNotice(ctx context.Context, o outgoing, r core.Room, target, text string) {
	latest, e := g.output(o.ID)
	if e != nil || (latest.State != "pending" && latest.State != "uncertain" && latest.State != "blocked") || !g.permittedOutput(latest, r) {
		return
	}
	var card core.Args
	_ = json.Unmarshal([]byte(jsonText(o.Card)), &card)
	if card == nil {
		card = DisplayCard("回复尚未完整送达", cut(o.Text, 23800))
	}
	card["header"] = core.Args{"template": "orange", "title": core.Args{"tag": "plain_text", "content": "回复尚未完整送达"}}
	body := argsOf(card["body"])
	elements, _ := body["elements"].([]any)
	body["elements"] = append(elements, core.Args{"tag": "markdown", "content": text + "\n/retry-output " + o.ID})
	card["body"] = body
	_ = providerVoid(ctx, func() error { return g.transport.Update(ctx, target, fitNoticeCard(card)) })
}
func (g *Gateway) retryable(room string) []string {
	const query = `SELECT o.id,o.at FROM outbox o WHERE o.room=? AND o.state='pending' AND o.retry_after<=? AND o.file_state!='uncertain' AND (o.target IS NOT NULL OR o.kind IN ('reply','recovery') OR (o.kind='file' AND o.file_state IN ('pending','done'))) AND (o.kind IN ('file','recovery') OR NOT EXISTS (SELECT 1 FROM outbox f WHERE f.room=o.room AND f.session=o.session AND f.kind='file' AND f.state NOT IN ('done','cancelled')))`
	g.outMu.Lock()
	cur, ok := g.outCursor[room]
	g.outMu.Unlock()
	read := func(after bool) ([]string, outputCursor) {
		q := query
		args := []any{room, time.Now().UnixMilli()}
		if after {
			q += ` AND (o.at>? OR (o.at=? AND o.id>?))`
			args = append(args, cur.At, cur.At, cur.ID)
		}
		q += ` ORDER BY o.at,o.id LIMIT 10`
		rows, e := g.store.DB.Query(q, args...)
		if e != nil {
			return nil, outputCursor{}
		}
		defer rows.Close()
		var ids []string
		var last outputCursor
		for rows.Next() {
			if rows.Scan(&last.ID, &last.At) == nil {
				ids = append(ids, last.ID)
			}
		}
		return ids, last
	}
	ids, last := read(ok)
	if len(ids) == 0 && ok {
		ids, last = read(false)
	}
	g.outMu.Lock()
	if len(ids) > 0 {
		g.outCursor[room] = last
	} else {
		delete(g.outCursor, room)
	}
	g.outMu.Unlock()
	return ids
}

type outputCursor struct {
	ID string
	At int64
}

func (g *Gateway) pruneExports(now int64) {
	rows, e := g.store.DB.Query(`SELECT id,file_path FROM outbox WHERE state IN ('done','cancelled') AND file_path IS NOT NULL AND at<? ORDER BY at LIMIT 100`, now-7*86400000)
	if e != nil {
		return
	}
	type item struct{ id, path string }
	var items []item
	for rows.Next() {
		var v item
		if rows.Scan(&v.id, &v.path) == nil {
			items = append(items, v)
		}
	}
	rows.Close()
	root := filepath.Join(g.config.DataDir, "exports")
	physicalRoot, e := filepath.EvalSymlinks(root)
	if e != nil {
		return
	}
	for _, v := range items {
		if !core.Inside(root, v.path) || filepath.Clean(v.path) == filepath.Clean(root) {
			continue
		}
		parent, e := filepath.EvalSymlinks(filepath.Dir(v.path))
		if e != nil || !core.Inside(physicalRoot, parent) {
			continue
		}
		if e = os.Remove(v.path); e != nil && !os.IsNotExist(e) {
			continue
		}
		_, _ = g.store.DB.Exec(`UPDATE outbox SET file_path=NULL WHERE id=? AND state IN ('done','cancelled') AND file_path=?`, v.id, v.path)
	}
}
func (g *Gateway) outputError(id string) string {
	return fmt.Sprintf("File not confirmed delivered. Check /outputs; explicit /retry-output %s may be required.", id)
}

func (g *Gateway) budgetOutput(o *outgoing) error {
	if o.Kind == "file" { return nil }
	card := o.Card
	if card == nil { card = DisplayCard("回复", o.Text) }
	size, err := core.CardPayloadBytes(card)
	if err != nil { return err }
	if size <= core.CardPayloadLimit { return nil }
	title := "回复"
	if !o.Success { title = "执行失败" }
	o.Card, _ = boundedReplyCard(title, o.Text, nil, 0, true, replyDetails{}, 0, o.Success, nil)
	if o.FilePath == "" {
		o.FileName = "reply.md"
		o.FilePath = filepath.Join(g.config.DataDir, "exports", core.Hash(o.Room)[:32], core.NewID()+".md")
		o.FileState = "pending"
	}
	_, err = g.store.DB.Exec("UPDATE outbox SET card=?,file_name=?,file_path=?,file_state=? WHERE id=?", jsonText(o.Card), o.FileName, o.FilePath, o.FileState, o.ID)
	return err
}

func (g *Gateway) outputCard(o outgoing, room core.Room, filesPending bool) core.Args {
	var card core.Args
	_ = json.Unmarshal([]byte(jsonText(o.Card)), &card)
	if card == nil {
		title := "回复"
		if !o.Success { title = "执行失败" }
		card = DisplayCard(title, o.Text)
	}
	card = g.withOutputControls(card, room, o.Session, o.ID)
	if filesPending {
		card["header"] = core.Args{"template":"orange", "title":core.Args{"tag":"plain_text", "content":"模型已完成 · 文件待送达"}}
		body := argsOf(card["body"])
		elements, _ := body["elements"].([]any)
		body["elements"] = append(elements, core.Args{"tag":"markdown", "content":"文件确认送达后才标记完成。可用 /outputs 查看状态。"})
		card["body"] = body
	}
	return fitNoticeCard(card)
}
