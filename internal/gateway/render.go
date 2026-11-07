package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type renderBlock struct {
	text  strings.Builder
	chars int
}

// A render has at most one provider request and one dirty snapshot. Slow card
// updates cannot accumulate closures or copies of every streamed token.
type render struct {
	mu                                                       sync.Mutex
	g                                                        *Gateway
	room                                                     core.Room
	session                                                  string
	ctx                                                      context.Context
	blocks                                                   map[string]*renderBlock
	order                                                    []string
	totalChars                                               int
	thinking                                                 map[string]string
	thinkingOrder                                            []string
	thinkingCount                                            int
	legacy                                                   int
	messageID, finalID, replyTo, threadID, status, last      string
	started                                                  int64
	details                                                  replyDetails
	createUncertain, createRejected, closed, flushing, dirty bool
	timer                                                    *time.Timer
	deliveryID                                               string
	turn                                                     string
	initialCard                                              core.Args
	boundTarget, boundKey                                    string
	waiting, failures                                        int
	retryAt                                                  time.Time
	blocked, urgent                                          bool
	done                                                     chan struct{}

	// detached marks a background result delivered after its originating
	// turn ended; it does not inherit that turn's delivery discard.
	detached bool
}

func newRender(g *Gateway, s *slot, anchor *core.Delivery) *render {
	r := &render{g: g, room: s.room, session: s.session.ID, ctx: s.ctx, blocks: map[string]*renderBlock{}, thinking: map[string]string{}, status: "处理中", deliveryID: "preview:" + core.NewID(), started: time.Now().UnixMilli(), details: replyDetails{Model: g.config.Model.Model, ProjectPath: s.workspace.Work}}
	if selected, err := config.ResolveSessionModel(g.config, s.session.Profile); err == nil {
		r.details.Model = selected.Model.Model
	}
	if anchor != nil {
		r.turn = anchor.UUID
		r.replyTo = messageReply(anchor.Message)
		r.threadID = anchor.Message.ThreadID
		r.started = anchor.Message.Time
	}
	return r
}
func (r *render) bind(anchor *core.Delivery) {
	if anchor == nil {
		return
	}
	r.mu.Lock()
	if r.messageID == "" && !r.flushing && !r.createUncertain {
		r.turn = anchor.UUID
		r.replyTo = messageReply(anchor.Message)
		r.threadID = anchor.Message.ThreadID
		r.started = anchor.Message.Time
	}
	r.mu.Unlock()
}
func (r *render) setDetails(d replyDetails) {
	r.mu.Lock()
	if d.Model != "" {
		r.details.Model = d.Model
	}
	if d.ProjectPath != "" {
		r.details.ProjectPath = d.ProjectPath
	}
	if d.Context != nil {
		r.details.Context = d.Context
	}
	if d.Usage != nil {
		r.details.Usage = d.Usage
	}
	r.mu.Unlock()
}
func (r *render) setStatus(text string) {
	r.mu.Lock()
	r.status = text
	r.urgent = true
	r.scheduleLocked()
	r.mu.Unlock()
}
func (r *render) update(text string, partial bool, kind, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil {
		return
	}
	if key == "" {
		key = fmt.Sprintf("%s:legacy:%d", kind, r.legacy)
		if !partial {
			r.legacy++
		}
	}
	if kind == "thinking" {
		old, exists := r.thinking[key]
		if !exists {
			r.thinkingCount++
			r.thinkingOrder = append(r.thinkingOrder, key)
		}
		if partial {
			text = old + text
		}
		r.thinking[key] = strings.Clone(cut(text, 180))
		for len(r.thinkingOrder) > 10 {
			delete(r.thinking, r.thinkingOrder[0])
			r.thinkingOrder = r.thinkingOrder[1:]
		}
	}
	if kind == "text" {
		block, exists := r.blocks[key]
		if !exists {
			block = &renderBlock{}
			r.blocks[key] = block
			r.order = append(r.order, key)
		}
		r.totalChars -= block.chars
		if !partial {
			block.text.Reset()
			block.chars = 0
		}
		remaining := 60000 - block.chars
		if remaining > 0 {
			text = cut(text, remaining)
			block.text.WriteString(text)
			block.chars += utf8.RuneCountInString(text)
		}
		r.totalChars += block.chars
		for len(r.order) > 256 || r.totalChars > 500000 {
			first := r.order[0]
			r.totalChars -= r.blocks[first].chars
			delete(r.blocks, first)
			r.order = r.order[1:]
		}
	}
	r.scheduleLocked()
}
func (r *render) answerLocked() string {
	parts := make([]string, 0, len(r.order))
	for _, id := range r.order {
		parts = append(parts, r.blocks[id].text.String())
	}
	return cut(core.Redact(strings.Join(parts, "\n\n"), r.g.secrets), 500000)
}
func (r *render) thinkingLocked() []string {
	steps := make([]string, 0, len(r.thinkingOrder))
	for _, id := range r.thinkingOrder {
		steps = append(steps, core.Redact(r.thinking[id], r.g.secrets))
	}
	return steps
}
func (r *render) cardFitLocked(title, answer string, final, success bool) (core.Args, bool) {
	d := r.details
	d.Model = core.Redact(d.Model, r.g.secrets)
	d.ProjectPath = core.Redact(d.ProjectPath, r.g.secrets)
	card, truncated := boundedReplyCard(title, answer, r.thinkingLocked(), r.thinkingCount, final, d, time.Duration(max(int64(0), time.Now().UnixMilli()-r.started))*time.Millisecond, success, r.g.quickControls(r.room, r.session, r.turn, r.finalID))
	if r.waiting > 0 && !final {
		cfg := argsOf(card["config"])
		cfg["streaming_mode"] = false
		delete(cfg, "streaming_config")
		card["config"] = cfg
	}
	return card, truncated
}
func (r *render) cardLocked(title, answer string, final, success bool) core.Args {
	card, _ := r.cardFitLocked(title, answer, final, success)
	return card
}
func (r *render) waitForAnswer(wait bool, title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if wait {
		r.waiting++
		// A permanent preview error must not hide an approval/question state. The
		// card still exists, so retry one status update; another permanent error
		// latches the preview again without touching the final delivery path.
		if r.blocked && r.messageID != "" {
			r.blocked = false
		}
	} else if r.waiting > 0 {
		r.waiting--
	}
	if r.waiting == 0 {
		title = "处理中"
	}
	r.status = title
	r.urgent = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.scheduleLocked()
}
func (r *render) scheduleLocked() {
	if r.closed || r.blocked || r.timer != nil || r.ctx.Err() != nil {
		return
	}
	if r.flushing {
		r.dirty = true
		return
	}
	delay := time.Duration(r.g.config.Limits.UpdateMS) * time.Millisecond
	if r.urgent || r.messageID == "" && !r.createUncertain && r.replyTo != "" {
		delay = 0
	}
	if remaining := time.Until(r.retryAt); remaining > delay {
		delay = remaining
	}
	r.urgent = false
	r.timer = time.AfterFunc(delay, func() {
		r.mu.Lock()
		r.timer = nil
		if r.closed || r.ctx.Err() != nil || r.replyTo == "" || r.createUncertain && r.messageID == "" {
			r.mu.Unlock()
			return
		}
		if r.flushing {
			// A timer that fired before Stop() can race with a newer timer. Only
			// one flush may own r.done; the running flush will reschedule.
			r.dirty = true
			r.mu.Unlock()
			return
		}
		r.flushing = true
		r.done = make(chan struct{})
		r.mu.Unlock()
		if !r.g.task(r.flush) {
			r.mu.Lock()
			r.flushing = false
			close(r.done)
			r.mu.Unlock()
		}
	})
}
func (r *render) flush() {
	r.mu.Lock()
	card := r.cardLocked(r.status, r.answerLocked(), false, false)
	target, reply, thread := r.messageID, r.replyTo, r.threadID
	// A logical create has immutable content and a durable UUID. Later deltas
	// are sent as an update only after the original message ID is confirmed.
	if target == "" {
		if r.initialCard == nil {
			r.initialCard = card
		}
		card = r.initialCard
	}
	snapshot := jsonText(card)
	same := snapshot == r.last
	ctx := core.WithDeliveryID(r.ctx, r.deliveryID)
	r.mu.Unlock()
	var err error
	if !same {
		if target == "" {
			target, err = provider(ctx, func() (string, error) { return r.g.transport.Card(ctx, r.room, card, reply, thread) })
			if err == nil && target == "" {
				err = errors.New("provider did not return message ID")
			}
		} else {
			// Serialize updates with outbox writers on the same message so a
			// cancellation status cannot be overwritten by a late preview flush.
			err = r.g.withOutputCard(ctx, target, func() error { return r.g.transport.Update(ctx, target, card) })
		}
	}
	r.mu.Lock()
	if err == nil {
		r.failures = 0
		r.retryAt = time.Time{}
		if target != "" && r.messageID == "" {
			r.messageID = target
			_ = r.g.store.RecordOutput(target, r.room.Key, r.session)
			if r.finalID != "" {
				if e := r.g.acknowledgePreviewTarget(r.ctx, r.finalID, r.room, target); e != nil {
					r.g.audit(r.room.Key, "preview_target_persistence_failed", core.Args{"output": r.finalID, "error": core.Redact(e.Error(), r.g.secrets)})
				}
			}
			// Include updates received during a rejected initial create.
			r.dirty = true
		}
		if target != "" {
			// The signed controls depend only on the target and turn/output
			// identity; rebinding an unchanged revision every flush would add a
			// SQLite transaction to the streaming hot path.
			key := r.turn + "\x00" + r.finalID
			if target != r.boundTarget || key != r.boundKey {
				if e := r.g.bindQuickControls(target, card); e == nil {
					r.boundTarget, r.boundKey = target, key
				}
			}
		}
		if r.ctx.Err() == nil {
			r.last = snapshot
		}
	} else {
		r.failures++
		var failure core.DeliveryFailure
		classified := errors.As(err, &failure)
		uncertain := !classified || failure.DeliveryUncertain()
		if r.messageID == "" && uncertain {
			r.createUncertain = true
		}
		if target == "" && classified && !failure.DeliveryRetryable() && !failure.DeliveryUncertain() {
			// The provider definitively rejected this preview create. Its frozen
			// payload and delivery key must not be reused for the final answer;
			// the outbox will create a card from the final snapshot instead.
			r.createRejected = true
		}
		retry := r.messageID != "" || classified && failure.DeliveryRetryable()
		if classified && !failure.DeliveryRetryable() && !failure.DeliveryUncertain() {
			retry = false
		}
		if retry && r.failures <= 4 && !r.createUncertain {
			delay := time.Duration(1<<min(r.failures-1, 5)) * time.Second
			if classified && failure.DeliveryRetryAfter() > delay {
				delay = failure.DeliveryRetryAfter()
			}
			r.retryAt = time.Now().Add(delay)
			r.dirty = true
		} else {
			r.blocked = true
		}
		if r.ctx.Err() == nil {
			r.g.audit(r.room.Key, "preview_delivery_pending", core.Args{"session": r.session, "attempt": r.failures, "uncertain": r.createUncertain})
		}
	}
	r.flushing = false
	close(r.done)
	dirty := r.dirty
	r.dirty = false
	if dirty && !r.closed {
		r.scheduleLocked()
	}
	r.mu.Unlock()
}

// persist commits the final reply and original inputs before waiting for an
// outstanding preview. The caller invokes it synchronously in the event reader.
func (r *render) persist(text string, success bool, who []core.Origin, title string, finished []string) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return func() {}, nil
	}
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	answer := core.Redact(text, r.g.secrets)
	if answer == "" {
		answer = r.answerLocked()
	}
	if title == "" {
		if success {
			title = "回复"
		} else {
			title = "执行失败"
		}
	}
	card, truncated := r.cardFitLocked(title, answer, true, success)
	name, path := "", ""
	if truncated {
		name = "reply.md"
		path = filepath.Join(r.g.config.DataDir, "exports", core.Hash(r.room.Key)[:32], core.NewID()+".md")
	}
	// A timer can reserve the in-flight slot before its task obtains this
	// mutex. Freeze its create here too, so the durable final output cannot
	// lose that preview's delivery key or immutable payload in the race.
	if r.flushing && r.messageID == "" && r.initialCard == nil {
		r.initialCard = r.cardLocked(r.status, r.answerLocked(), false, false)
	}
	state := "pending"
	if r.flushing {
		state = "preparing"
	} else if r.createUncertain && r.messageID == "" {
		state = "uncertain"
	}
	id := ""
	err := r.g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		var e error
		id, e = r.g.saveOutputWith(tx, r.room, r.session, answer, success, r.messageID, r.replyTo, r.threadID, card, who, name, path, "reply", state, !r.detached)
		if e != nil {
			return e
		}
		if r.initialCard != nil && !r.createRejected {
			if _, e = tx.Exec("UPDATE outbox SET delivery_key=?,initial_card=? WHERE id=?", r.deliveryID, jsonText(r.initialCard), id); e != nil {
				return e
			}
		}
		for _, uuid := range finished {
			if _, e = tx.Exec("UPDATE inbox SET status='done',payload=NULL WHERE uuid=? AND room=? AND session=? AND status IN ('sent','consumed')", uuid, r.room.Key, r.session); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		// Nothing was committed: leave the render open so the caller can retry
		// the same final reply instead of treating later turns as finished.
		r.closed = false
		return nil, err
	}
	r.finalID = id
	pending := r.flushing
	done := r.done
	return func() {
		if pending {
			<-done
		}
		r.mu.Lock()
		target, uncertain := r.messageID, r.createUncertain
		r.mu.Unlock()
		if state == "preparing" {
			next := "pending"
			if target == "" && uncertain {
				next = "uncertain"
			}
			_, e := r.g.store.DB.Exec("UPDATE outbox SET target=?,state=? WHERE id=? AND state='preparing'", nullString(target), next, id)
			if e != nil {
				return
			}
		}
		if r.ctx.Err() == nil {
			if discarded, err := r.g.closeDiscardedOutput(r.ctx, id, r.room, target); err != nil || discarded {
				return
			}
			_, _ = r.g.deliverOutput(r.ctx, id, r.room, false)
		}
	}, nil
}
func (r *render) cancel(title string) {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	pending, done := r.flushing, r.done
	r.mu.Unlock()
	if pending {
		<-done
	}
	r.mu.Lock()
	id := r.messageID
	answer := "以下为中断前输出，不代表任务完成。\n\n" + r.answerLocked()
	card := replaceQuickControls(r.cardLocked(title, answer, true, false), nil)
	body := argsOf(card["body"])
	if elements, ok := body["elements"].([]any); ok {
		for _, item := range elements {
			element := argsOf(item)
			if stringArg(element, "element_id") == "reply_answer" {
				element["content"] = strings.ReplaceAll(stringArg(element, "content"), "（完整答案另附文件。）", "（中断前输出预览已截断。）")
			}
		}
	}
	card = fitCardBudget(card, core.CardPayloadLimit-outputStatusReserve, "\n\n（中断前输出预览已截断。）")
	r.mu.Unlock()
	if id == "" {
		return
	}
	// Finalizing an existing card is durable too: failed /stop, /new or
	// shutdown cosmetics must not leave an endlessly streaming orphan.
	output, err := r.g.saveOutput(r.g.store.DB, r.room, r.session, answer, false, id, r.replyTo, r.threadID, card, nil, "", "", "status", "pending")
	if err != nil {
		r.g.audit(r.room.Key, "card_close_pending", core.Args{"session": r.session})
		return
	}
	if !r.g.isClosing() {
		r.g.task(func() { _, _ = r.g.deliverOutput(r.g.ctx, output, r.room, false) })
	}
}
