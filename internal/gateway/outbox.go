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
	QueryRow(string, ...any) *sql.Row
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

// Manual artifact retrieval has its own file delivery record. It must not
// become a required attachment for a later native turn in the same session.
func (o outgoing) fileOnly() bool { return o.Kind == "file" || o.Kind == "artifact" }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (g *Gateway) saveOutput(db sqlExec, r core.Room, session, text string, success bool, target, replyTo, thread string, card core.Args, who []core.Origin, filename, path, kind, state string) (string, error) {
	return g.saveOutputWith(db, r, session, text, success, target, replyTo, thread, card, who, filename, path, kind, state, true)
}

// saveOutputWith lets a detached background result keep its origin for
// ownership without inheriting a discard recorded for that earlier turn.
func (g *Gateway) saveOutputWith(db sqlExec, r core.Room, session, text string, success bool, target, replyTo, thread string, card core.Args, who []core.Origin, filename, path, kind, state string, inheritDiscard bool) (string, error) {
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
	_, e := db.Exec(`INSERT INTO outbox(id,room,session,text,target,reply_to,thread_id,success,state,at,card,origins,file_name,file_path,file_state,kind) VALUES(?,?,?,?,?,?,?,?,CASE WHEN ? AND ? IN ('reply','file') AND EXISTS(SELECT 1 FROM output_discarded_origins d WHERE d.room=? AND d.session=? AND d.origin IN (SELECT json_extract(value,'$.id') FROM json_each(?))) THEN 'cancelled' ELSE ? END,?,?,?,?,?,?,?)`, id, r.Key, session, text, nullString(target), nullString(replyTo), nullString(thread), success, inheritDiscard, kind, r.Key, session, jsonText(who), state, time.Now().UnixMilli(), cardJSON, jsonText(who), nullString(filename), nullString(path), fileState, kind)
	if e == nil && inheritDiscard {
		e = g.inheritedOutputDiscard(db, id)
	}
	return id, e
}
func (g *Gateway) output(id string) (outgoing, error) {
	return g.outputContext(context.Background(), id)
}
func (g *Gateway) outputContext(ctx context.Context, id string) (outgoing, error) {
	var o outgoing
	var target, reply, thread, card, name, path, ft, raw, key, initial sql.NullString
	err := g.store.DB.QueryRowContext(ctx, `SELECT id,kind,room,session,text,target,reply_to,thread_id,success,state,card,origins,file_name,file_path,file_target,file_state,delivery_key,initial_card,retry_count,retry_after FROM outbox WHERE id=?`, id).Scan(&o.ID, &o.Kind, &o.Room, &o.Session, &o.Text, &target, &reply, &thread, &o.Success, &o.State, &card, &raw, &name, &path, &ft, &o.FileState, &key, &initial, &o.RetryCount, &o.RetryAfter)
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
	if o.DeliveryKey == "" {
		o.DeliveryKey = "output:" + o.ID
	}
	if initial.Valid && initial.String != "" {
		if err = json.Unmarshal([]byte(initial.String), &o.InitialCard); err != nil {
			return o, err
		}
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
	return g.outputAllowedContext(context.Background(), o, r)
}
func (g *Gateway) outputAllowedContext(ctx context.Context, o outgoing, r core.Room) bool {
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
	m, err := g.store.ContextMessageContext(ctx, r.Key, o.ReplyTo)
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

// filesPendingFor reports whether an unfinished file belongs to the same turn
// as o. File and reply rows persist the triggering origins, so a permanently
// failed attachment from an earlier turn must not defer later replies in the
// same session. A reply without origins keeps the conservative session-wide
// behavior because its turn cannot be attributed.
func (g *Gateway) filesPendingFor(o outgoing) (bool, error) {
	if o.Kind != "reply" || !o.Success {
		return false, nil
	}
	rows, err := g.store.DB.Query(`SELECT origins FROM outbox WHERE room=? AND session=? AND kind='file' AND state NOT IN ('done','cancelled')`, o.Room, o.Session)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	replyOrigins := originIDs(o.Origins)
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		if len(replyOrigins) == 0 {
			return true, nil
		}
		var fileOrigins []core.Origin
		if raw.Valid && raw.String != "" && raw.String != "null" {
			_ = json.Unmarshal([]byte(raw.String), &fileOrigins)
		}
		for _, origin := range fileOrigins {
			if _, ok := replyOrigins[origin.ID]; ok {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func originIDs(origins []core.Origin) map[string]struct{} {
	ids := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if origin.ID != "" {
			ids[origin.ID] = struct{}{}
		}
	}
	return ids
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
	if (o.State == "pending" || !explicit) && o.RetryAfter > time.Now().UnixMilli() {
		return false, nil
	}
	ctx = core.WithDeliveryID(ctx, o.DeliveryKey)
	if explicit {
		ctx = core.WithExplicitRetry(ctx)
	}
	if err = g.budgetOutput(&o, r); err != nil {
		return false, err
	}
	result, err := g.store.DB.Exec(`UPDATE outbox SET state='sending',delivery_error=NULL WHERE id=? AND state=?`, id, o.State)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return false, nil
	}
	// The row is in flight now. Reflect that in the in-memory copy so an
	// intermediate card never advertises a retry for work that is still being
	// delivered; failed attempts reload the durable state before rendering it.
	o.State = "sending"
	deliveryAttempt := o.RetryCount + 1
	started := time.Now()
	defer func() { g.auditTurnDelivery(id, o, deliveryAttempt, started) }()
	target := o.Target
	phase := "card"
	knownFile := o.FileState == "done" && o.FileTarget != ""
	if o.fileOnly() {
		phase = "file_prepare"
	}
	perform := func() error {
		// Capture the pending-attachment decision once. Re-querying after the
		// card update could observe a file that completed mid-update and skip
		// both the deferral and the terminal card update.
		otherFilesPending, e := g.filesPendingFor(o)
		if e != nil {
			return e
		}
		if !o.fileOnly() {
			filesPending := (o.FileName != "" && !knownFile) || otherFilesPending
			card := g.outputCard(o, r, filesPending)
			if target == "" {
				if o.InitialCard == nil {
					o.InitialCard = card
					res, e := g.store.DB.Exec("UPDATE outbox SET initial_card=? WHERE id=? AND state='sending'", jsonText(card), id)
					if e != nil {
						return e
					}
					if n, _ := res.RowsAffected(); n != 1 {
						return errCancelledDelivery
					}
				}
				if !g.outputSending(id, r) {
					return errCancelledDelivery
				}
				target, err = provider(ctx, func() (string, error) { return g.transport.Card(ctx, r, o.InitialCard, o.ReplyTo, o.ThreadID) })
				if err != nil {
					return err
				}
				if target != "" {
					if _, err = g.store.DB.Exec("UPDATE outbox SET target=? WHERE id=? AND state='sending'", target, id); err != nil {
						return err
					}
				}
			}
			if !g.outputSending(id, r) {
				return errCancelledDelivery
			}
			updateCtx := ctx
			if !filesPending {
				updateCtx = core.WithTerminalCard(ctx)
			}
			if target != "" {
				err = g.updateOutputCard(updateCtx, id, r, target, card)
				if err != nil {
					return err
				}
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
		}
		if (o.fileOnly() || o.FileName != "") && !knownFile {
			phase = "file_prepare"
			if o.FilePath == "" || o.FileName == "" {
				return errors.New("missing export snapshot")
			}
			if !o.fileOnly() {
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
			fileCtx := core.WithDeliveryID(ctx, "file:"+o.ID)
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
		} else if o.fileOnly() {
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
		if otherFilesPending {
			_, e := g.store.DB.Exec(`UPDATE outbox SET state='pending',delivery_error=? WHERE id=? AND state='sending'`, "Attachment delivery remains pending.", id)
			if e != nil {
				return e
			}
			g.pendingOutputNotice(ctx, o, r, target, "主动发送的文件尚未确认送达；本轮未标记完成。可用 /outputs 查看记录。")
			return errDeferredDelivery
		}
		if !o.fileOnly() && o.FileName != "" && knownFile {
			phase = "card_finalize"
			card := g.deliveredOutputCard(o, r)
			finalCtx := core.WithTerminalCard(ctx)
			if e := g.updateOutputCard(finalCtx, id, r, target, card); e != nil {
				return e
			}
		}
		// A text-only reply has no later card_finalize step, so drop the retry
		// action here. The answer is already delivered; a failed cosmetic refresh
		// must not turn it into a failed delivery, but cancellation still wins.
		if !o.fileOnly() && o.Kind != "status" && !(o.FileName != "" && knownFile) {
			card := g.deliveredOutputCard(o, r)
			if e := g.updateOutputCard(core.WithTerminalCard(ctx), id, r, target, card); e != nil {
				if errors.Is(e, errCancelledDelivery) {
					return e
				}
				g.audit(r.Key, "output_controls_refresh_failed", core.Args{"output": o.ID, "error": e.Error()})
			}
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
	if classified {
		detail := cut(core.Redact(err.Error(), g.secrets), 500)
		if failure.DeliveryUncertain() {
			reason += " " + detail
		} else {
			reason = detail
			if !failure.DeliveryRetryable() {
				state = "blocked"
			}
		}
	}
	attempt := o.RetryCount + 1
	if state == "pending" && attempt >= 6 {
		state = "blocked"
		reason += " Automatic retry budget exhausted; resolve the cause and use /retry-output."
	}
	delay := time.Duration(1<<min(attempt, 8)) * time.Second
	if classified && failure.DeliveryRetryAfter() > delay {
		delay = failure.DeliveryRetryAfter()
	}
	fileState := ""
	if phase == "file_send" && !knownFile {
		fileState = "pending"
		if state == "uncertain" {
			fileState = "uncertain"
		}
	}
	_, dbErr := g.store.DB.Exec(`UPDATE outbox SET state=?,target=COALESCE(?,target),file_state=CASE WHEN ?!='' THEN ? ELSE file_state END,delivery_error=?,retry_count=?,retry_after=? WHERE id=? AND state='sending'`, state, nullString(target), fileState, fileState, reason, attempt, time.Now().Add(delay).UnixMilli(), id)
	g.audit(r.Key, "output_delivery_failed", core.Args{"id": id, "phase": "delivery", "stage": phase, "reason": failureReason(err), "kind": o.Kind, "knownTarget": target != "", "knownFile": knownFile})
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
	_ = g.withOutputCard(ctx, target, func() error {
		latest, e := g.output(o.ID)
		if e != nil || latest.Target != target || (latest.State != "pending" && latest.State != "uncertain" && latest.State != "blocked") || !g.permittedOutput(latest, r) {
			return errCancelledDelivery
		}
		card := g.outputCard(latest, r, true)
		card = setReplyCardStatus(card, g.cardTitle(), "回复尚未完整送达", "orange")
		body := argsOf(card["body"])
		elements, _ := body["elements"].([]any)
		body["elements"] = append(elements, core.Args{"tag": "markdown", "content": text + "\n/retry-output " + o.ID})
		card["body"] = body
		card = fitNoticeCard(card)
		if err := g.transport.Update(ctx, target, card); err != nil {
			return err
		}
		return g.bindQuickControls(target, card)
	})
}

// Hold the target lock inside the provider call: cancellation can return to the
// caller before a slow transport finishes, but a closing status must stay after
// that actual write. No SQLite transaction waits for this lock.
func (g *Gateway) withOutputCard(ctx context.Context, target string, write func() error) error {
	return providerVoid(ctx, func() error {
		unlock, err := g.outTargetLocks.lockContext(ctx, target)
		if err != nil {
			return err
		}
		defer unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		return write()
	})
}

func (g *Gateway) updateOutputCard(ctx context.Context, id string, room core.Room, target string, card core.Args) error {
	return g.withOutputCard(ctx, target, func() error {
		current, err := g.output(id)
		if err != nil {
			return err
		}
		if current.State != "sending" || current.Target != target || current.Room != room.Key || !g.permittedOutput(current, room) {
			return errCancelledDelivery
		}
		if err := g.transport.Update(ctx, target, card); err != nil {
			return err
		}
		if !g.outputSending(id, room) {
			return errCancelledDelivery
		}
		return g.bindQuickControls(target, card)
	})
}

const retryCandidateLimit = 128
const retryCandidateQuery = `SELECT o.id,o.at,o.retry_after,o.file_state,o.kind,o.target IS NOT NULL FROM outbox o WHERE o.room=? AND o.state='pending'`

func (g *Gateway) retryable(room string) []string {
	g.outMu.Lock()
	cur, ok := g.outCursor[room]
	g.outMu.Unlock()
	ctx, cancel := context.WithTimeout(g.ctx, 500*time.Millisecond)
	defer cancel()
	now := time.Now().UnixMilli()
	read := func(after bool) ([]string, outputCursor, int, error) {
		q := retryCandidateQuery
		args := []any{room}
		if after {
			q += ` AND (o.at,o.id)>(?,?)`
			args = append(args, cur.At, cur.ID)
		}
		q += ` ORDER BY o.at,o.id LIMIT ?`
		args = append(args, retryCandidateLimit)
		rows, e := g.store.DB.QueryContext(ctx, q, args...)
		if e != nil {
			return nil, outputCursor{}, 0, e
		}
		defer rows.Close()
		var ids []string
		var last outputCursor
		scanned := 0
		for rows.Next() {
			var retryAfter int64
			var fileState, kind string
			var hasTarget bool
			if e = rows.Scan(&last.ID, &last.At, &retryAfter, &fileState, &kind, &hasTarget); e != nil {
				return nil, outputCursor{}, 0, e
			}
			scanned++
			if retryAfter > now || fileState == "uncertain" || !(hasTarget || kind == "reply" || kind == "recovery" || (kind == "file" || kind == "artifact") && (fileState == "pending" || fileState == "done")) {
				continue
			}
			ids = append(ids, last.ID)
			if len(ids) == 10 {
				break
			}
		}
		return ids, last, scanned, rows.Err()
	}
	ids, last, scanned, err := read(ok)
	if err == nil && scanned == 0 && ok {
		ids, last, scanned, err = read(false)
	}
	if err != nil {
		if g.logger != nil {
			g.logger("output_retry_scan_failed", core.Args{"room": room, "error": core.Redact(err.Error(), g.secrets)})
		}
		return nil
	}
	g.outMu.Lock()
	if scanned > 0 {
		// Progress follows candidates, including those not due or waiting on
		// attachments, so a protected prefix cannot starve later outputs.
		g.outCursor[room] = last
	} else {
		delete(g.outCursor, room)
	}
	g.outMu.Unlock()
	return g.readyForDelivery(ids)
}

// readyForDelivery drops replies whose own-turn attachments are still pending.
// The cursor still advances over the raw batch so filtered rows cannot starve
// later candidates; once the attachment finishes the reply is selected again.
func (g *Gateway) readyForDelivery(ids []string) []string {
	ready := make([]string, 0, len(ids))
	for _, id := range ids {
		o, err := g.output(id)
		if err != nil {
			continue
		}
		pending, err := g.filesPendingFor(o)
		if err != nil || pending {
			continue
		}
		ready = append(ready, id)
	}
	return ready
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

// Reserve room for later delivery-state notices before deciding whether the
// answer needs a file. Adding controls after this decision must never silently
// truncate a legacy outbox answer whose only copy was the card.
const outputStatusReserve = 2048

func (g *Gateway) budgetOutput(o *outgoing, room core.Room) error {
	if o.fileOnly() {
		return nil
	}
	card := o.Card
	if card == nil {
		card = DisplayCard("回复", o.Text)
	}
	if o.Kind == "status" {
		card = replaceQuickControls(card, nil)
	} else {
		card = g.withOutputControls(card, room, o.Session, o.ID, o.RetryCount)
	}
	budget := core.CardPayloadLimit - outputStatusReserve
	size, err := core.CardPayloadBytes(card)
	if err != nil {
		return err
	}
	if size <= budget {
		return nil
	}
	suffix := "\n\n（完整答案另附文件。）"
	if o.Kind == "status" {
		suffix = "\n\n（中断前输出预览已截断。）"
	}
	o.Card = fitCardBudget(card, budget, suffix)
	if size, err := core.CardPayloadBytes(o.Card); err != nil || size > budget {
		// Legacy/custom metadata can itself exceed the limit. Preserve the full
		// answer in the export and use a minimal template to unblock delivery.
		if o.Kind == "status" {
			card = cancelledOutputCardTitle(g.cardTitle(), nil, o.Text)
		} else {
			title := "回复"
			if !o.Success {
				title = "执行失败"
			}
			card = DisplayCard(title, o.Text)
			card["config"] = core.Args{"streaming_mode": false}
			card = g.withOutputControls(card, room, o.Session, o.ID, o.RetryCount)
		}
		o.Card = fitCardBudget(card, budget, suffix)
	}
	if o.Kind == "status" {
		_, err = g.store.DB.Exec("UPDATE outbox SET card=? WHERE id=? AND state NOT IN ('done','cancelled')", jsonText(o.Card), o.ID)
		return err
	}
	if o.FilePath == "" {
		o.FileName = "reply.md"
		o.FilePath = filepath.Join(g.config.DataDir, "exports", core.Hash(o.Room)[:32], core.NewID()+".md")
		o.FileState = "pending"
	}
	_, err = g.store.DB.Exec("UPDATE outbox SET card=?,file_name=?,file_path=?,file_state=? WHERE id=? AND state NOT IN ('done','cancelled')", jsonText(o.Card), o.FileName, o.FilePath, o.FileState, o.ID)
	return err
}

// deliveredOutputCard rebuilds an output card after every delivery step has
// succeeded. Session-level actions stay available, while the retry action is
// dropped because there is no longer an incomplete delivery to retry.
func (g *Gateway) deliveredOutputCard(o outgoing, r core.Room) core.Args {
	card := g.outputCard(o, r, false)
	controls := g.quickControls(r, o.Session, "", "")
	if o.Kind == "recovery" && g.resumeAvailable(r, o.Session) {
		controls = append(controls, g.resumeControls(r, o.Session, g.quickRevision(r, o.Session, "", "", ""))...)
	}
	if o.Kind == "reply" && o.Success && g.feedbackEnabled(r) && replyCardDone(card) {
		controls = append(controls, g.feedbackControls(r, o.Session, o.ID, g.quickRevision(r, o.Session, "", "", ""))...)
	}
	card = replaceQuickControls(card, controls)
	return fitNoticeCard(card)
}

func (g *Gateway) outputCard(o outgoing, room core.Room, filesPending bool) core.Args {
	var card core.Args
	_ = json.Unmarshal([]byte(jsonText(o.Card)), &card)
	if card == nil {
		title := "回复"
		if !o.Success {
			title = "执行失败"
		}
		card = DisplayCard(title, o.Text)
	}
	if o.Kind == "status" {
		card = replaceQuickControls(card, nil)
		if g.resumeAvailable(room, o.Session) {
			card = replaceQuickControls(card, g.resumeControls(room, o.Session, g.quickRevision(room, o.Session, "", "", "")))
		}
	} else if o.State == "pending" || o.State == "uncertain" || o.State == "blocked" {
		// Only a delivery that can still be retried gets the retry control.
		// An in-flight delivery that later succeeds must not leave a stale retry
		// button behind when the final cosmetic card refresh is rejected.
		card = g.withOutputControls(card, room, o.Session, o.ID, o.RetryCount)
	} else {
		card = replaceQuickControls(card, g.quickControls(room, o.Session, "", ""))
	}
	if filesPending {
		card = setReplyCardStatus(card, g.cardTitle(), "模型已完成 · 文件待送达", "orange")
		body := argsOf(card["body"])
		elements, _ := body["elements"].([]any)
		body["elements"] = append(elements, core.Args{"tag": "markdown", "content": "文件确认送达后才标记完成。可用 /outputs 查看状态。"})
		card["body"] = body
	}
	return fitNoticeCard(card)
}
