package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type renderBlock struct{ kind, text string }

// A render has at most one provider request and one dirty snapshot. Slow card
// updates cannot accumulate closures or copies of every streamed token.
type render struct {
	mu                                                  sync.Mutex
	g                                                   *Gateway
	room                                                core.Room
	session                                             string
	ctx                                                 context.Context
	blocks                                              map[string]renderBlock
	order                                               []string
	totalChars                                          int
	thinking                                            map[string]string
	thinkingOrder                                       []string
	thinkingCount                                       int
	legacy                                              int
	messageID, finalID, replyTo, threadID, status, last string
	started                                             int64
	details                                             replyDetails
	createUncertain, closed, flushing, dirty            bool
	timer                                               *time.Timer
	done                                                chan struct{}
}

func newRender(g *Gateway, s *slot, anchor *core.Delivery) *render {
	r := &render{g: g, room: s.room, session: s.session.ID, ctx: s.ctx, blocks: map[string]renderBlock{}, thinking: map[string]string{}, status: "处理中", started: time.Now().UnixMilli(), details: replyDetails{Model: g.config.Model.Model, ProjectPath: s.workspace.Work}}
	if anchor != nil {
		r.replyTo = anchor.Message.ID
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
		r.replyTo = anchor.Message.ID
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
		r.thinking[key] = cut(text, 180)
		for len(r.thinkingOrder) > 10 {
			delete(r.thinking, r.thinkingOrder[0])
			r.thinkingOrder = r.thinkingOrder[1:]
		}
	}
	if kind == "text" {
		old, exists := r.blocks[key]
		if !exists {
			r.order = append(r.order, key)
		}
		if partial {
			text = old.text + text
		}
		text = cut(text, 60000)
		r.totalChars -= len([]rune(old.text))
		r.blocks[key] = renderBlock{kind, text}
		r.totalChars += len([]rune(text))
		for len(r.order) > 256 || r.totalChars > 500000 {
			first := r.order[0]
			r.totalChars -= len([]rune(r.blocks[first].text))
			delete(r.blocks, first)
			r.order = r.order[1:]
		}
	}
	r.scheduleLocked()
}
func (r *render) answerLocked() string {
	parts := make([]string, 0, len(r.order))
	for _, id := range r.order {
		parts = append(parts, r.blocks[id].text)
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
func (r *render) cardLocked(title, answer string, final, success bool) core.Args {
	d := r.details
	d.Model = core.Redact(d.Model, r.g.secrets)
	d.ProjectPath = core.Redact(d.ProjectPath, r.g.secrets)
	return replyCard(title, answer, r.thinkingLocked(), r.thinkingCount, final, d, time.Duration(max(int64(0), time.Now().UnixMilli()-r.started))*time.Millisecond, success)
}
func (r *render) scheduleLocked() {
	if r.closed || r.timer != nil || r.ctx.Err() != nil {
		return
	}
	if r.flushing {
		r.dirty = true
		return
	}
	delay := time.Duration(r.g.config.Limits.UpdateMS) * time.Millisecond
	if r.messageID == "" && !r.createUncertain && r.replyTo != "" {
		delay = 0
	}
	r.timer = time.AfterFunc(delay, func() {
		r.mu.Lock()
		r.timer = nil
		if r.closed || r.ctx.Err() != nil || r.replyTo == "" || r.createUncertain && r.messageID == "" {
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
	snapshot := jsonText(card)
	target, reply, thread := r.messageID, r.replyTo, r.threadID
	same := snapshot == r.last
	r.mu.Unlock()
	var err error
	if !same {
		if target == "" {
			target, err = provider(r.ctx, func() (string, error) { return r.g.transport.Card(r.ctx, r.room, card, reply, thread) })
			if err == nil && target == "" {
				err = errors.New("provider did not return message ID")
			}
		} else {
			err = providerVoid(r.ctx, func() error { return r.g.transport.Update(r.ctx, target, card) })
		}
	}
	r.mu.Lock()
	if err == nil {
		if target != "" && r.messageID == "" {
			r.messageID = target
			_ = r.g.store.RecordOutput(target, r.room.Key, r.session)
			if r.finalID != "" {
				_, _ = r.g.store.DB.Exec("UPDATE outbox SET target=? WHERE id=? AND state='preparing'", target, r.finalID)
			}
		}
		if r.ctx.Err() == nil {
			r.last = snapshot
		}
	} else {
		if r.messageID == "" {
			r.createUncertain = true
		}
		if r.ctx.Err() == nil {
			r.g.audit(r.room.Key, "preview_delivery_pending", core.Args{"session": r.session})
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
	body := answer
	name, path := "", ""
	if len([]rune(answer)) > 24000 {
		body = cut(answer, 23800) + "\n\n（完整答案另附文件。）"
		name = "reply.md"
		path = filepath.Join(r.g.config.DataDir, "exports", core.Hash(r.room.Key)[:32], core.NewID()+".md")
	}
	card := r.cardLocked(title, body, true, success)
	state := "pending"
	if r.flushing {
		state = "preparing"
	} else if r.createUncertain && r.messageID == "" {
		state = "uncertain"
	}
	id := ""
	err := r.g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		var e error
		id, e = r.g.saveOutput(tx, r.room, r.session, answer, success, r.messageID, r.replyTo, r.threadID, card, who, name, path, "reply", state)
		if e != nil {
			return e
		}
		for _, uuid := range finished {
			if _, e = tx.Exec("UPDATE inbox SET status='done',payload=NULL WHERE uuid=? AND room=? AND session=? AND status IN ('sent','consumed')", uuid, r.room.Key, r.session); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
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
	card := r.cardLocked(title, "以下为中断前输出，不代表任务完成。\n\n"+r.answerLocked(), true, false)
	r.mu.Unlock()
	if id != "" && !r.g.isClosing() {
		r.g.task(func() {
			ctx, cancel := withTimeout()
			defer cancel()
			_ = providerVoid(ctx, func() error { return r.g.transport.Update(ctx, id, card) })
		})
	}
}
