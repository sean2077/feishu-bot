package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"sort"
	"strings"
	"time"
)

type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
type question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []questionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
}
type approval struct {
	room                                   core.Room
	session, kind, name, summary, revision string
	expires                                int64
	cards                                  map[string]string
	forwarded                              bool
	questions                              []question
	selected                               map[string]map[int]map[int]bool
	outcome                                chan core.Args
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	timer                                  *time.Timer
	detach                                 func() bool
}

func parseQuestions(raw any) ([]question, bool) {
	b, e := json.Marshal(raw)
	if e != nil {
		return nil, false
	}
	var qs []question
	if e = json.Unmarshal(b, &qs); e != nil || len(qs) == 0 || len(qs) > 8 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, q := range qs {
		if q.Question == "" || seen[q.Question] || len(q.Options) > 12 {
			return nil, false
		}
		seen[q.Question] = true
		labels := map[string]bool{}
		for _, o := range q.Options {
			if labels[o.Label] {
				return nil, false
			}
			labels[o.Label] = true
		}
	}
	return qs, true
}
func (g *Gateway) approvalCount(session string) int {
	g.approvalMu.Lock()
	defer g.approvalMu.Unlock()
	n := 0
	for _, p := range g.approvals {
		if p.session == session {
			n++
		}
	}
	return n
}
func (g *Gateway) ask(ctx context.Context, r core.Room, session, name string, input core.Args, summary string, questions []question) (core.Args, error) {
	if ctx.Err() != nil {
		return core.Args{"approved": false, "reason": "cancelled"}, nil
	}
	kind := "permission"
	if questions != nil {
		kind = "question"
	}
	id := core.NewID()
	expires := time.Now().UnixMilli() + g.config.Limits.ApprovalMS
	waitCtx, cancel := context.WithCancel(ctx)
	p := &approval{room: r, session: session, kind: kind, name: name, summary: summary, revision: core.NewID(), expires: expires, cards: map[string]string{}, forwarded: kind == "permission" && r.DMUser != "" && !contains(r.Maintainers, r.DMUser), questions: questions, selected: map[string]map[int]map[int]bool{}, outcome: make(chan core.Args, 1), ctx: waitCtx, cancel: cancel}
	g.approvalMu.Lock()
	if len(g.approvals) >= 256 {
		g.approvalMu.Unlock()
		cancel()
		return core.Args{"approved": false, "reason": "too many pending requests"}, nil
	}
	_, e := g.store.DB.Exec("INSERT INTO requests(id,room,session,kind,digest,state,expires) VALUES(?,?,?,?,?,'pending',?)", id, r.Key, session, kind, core.Hash(jsonText(core.Args{"name": name, "input": input})), expires)
	if e != nil {
		g.approvalMu.Unlock()
		cancel()
		return nil, e
	}
	g.approvals[id] = p
	p.timer = time.AfterFunc(time.Duration(g.config.Limits.ApprovalMS)*time.Millisecond, func() { g.settleApproval(id, core.Args{"approved": false, "reason": "expired"}, "system") })
	p.detach = context.AfterFunc(ctx, func() { g.settleApproval(id, core.Args{"approved": false, "reason": "cancelled"}, "system") })
	card := g.redactArgs(approvalCard(id, p))
	g.approvalMu.Unlock()
	targets := []core.Room{r}
	if p.forwarded {
		targets = nil
		for _, id := range r.Maintainers {
			target := r
			target.ChatID = id
			target.DMUser = id
			targets = append(targets, target)
		}
		g.notify(r, "这项操作需要项目维护者批准。已向维护者私聊发送操作摘要与参数；不会转发聊天历史。", nil)
	}
	g.task(func() {
		success := false
		for _, target := range targets {
			if waitCtx.Err() != nil {
				break
			}
			mid, e := provider(waitCtx, func() (string, error) {
				mid, e := g.transport.Card(waitCtx, target, card, "", "")
				if e == nil && waitCtx.Err() != nil {
					ctx, cancel := withTimeout()
					defer cancel()
					_ = g.transport.Update(ctx, mid, DisplayCard("请求已结束", "该请求不再接受操作。"))
				}
				return mid, e
			})
			if e != nil || mid == "" {
				continue
			}
			g.approvalMu.Lock()
			alive := g.approvals[id] == p
			if alive {
				owner := ""
				if p.forwarded {
					owner = target.DMUser
				}
				p.cards[mid] = owner
			}
			g.approvalMu.Unlock()
			if !alive {
				unlock := g.approvalUpdateLocks.lock(mid)
				cosmetic, cancelCosmetic := withTimeout()
				_ = providerVoid(cosmetic, func() error {
					return g.transport.Update(cosmetic, mid, DisplayCard("请求已结束", "该请求不再接受操作。"))
				})
				cancelCosmetic()
				unlock()
			}
			if alive {
				success = true
				if !p.forwarded {
					_ = g.store.RecordOutput(mid, r.Key, session)
				}
			}
		}
		if !success {
			g.settleApproval(id, core.Args{"approved": false, "reason": "card delivery failed"}, "system")
		}
	})
	if ctx.Err() != nil {
		g.settleApproval(id, core.Args{"approved": false, "reason": "cancelled"}, "system")
	}
	select {
	case result := <-p.outcome:
		if ctx.Err() != nil {
			return core.Args{"approved": false, "reason": "cancelled"}, nil
		}
		return result, nil
	case <-ctx.Done():
		// The cancellation callback attempts durable settlement. Its SQLite
		// failure cannot keep a native control request waiting indefinitely.
		return core.Args{"approved": false, "reason": "cancelled"}, nil
	}
}
func approvalCard(id string, p *approval) core.Args {
	if p.kind == "permission" {
		return DisplayCard("需要维护者批准", p.summary+"\n\n仅批准这一次操作，超时或停止后失效。\n请求：`"+id+"`", button("批准一次", core.Args{"request": id, "decision": "allow"}, "primary"), button("拒绝", core.Args{"request": id, "decision": "deny"}, "danger"))
	}
	elements := []any{}
	for qi, q := range p.questions {
		mode := "单选"
		if q.MultiSelect {
			mode = "多选"
		}
		header := q.Header
		if header != "" {
			header += " · "
		}
		elements = append(elements, core.Args{"tag": "markdown", "content": fmt.Sprintf("**%d. %s%s**\n%s", qi+1, header, mode, q.Question)})
		for oi, o := range q.Options {
			if o.Description != "" {
				elements = append(elements, core.Args{"tag": "markdown", "content": fmt.Sprintf("%d.%d %s\n%s", qi+1, oi+1, o.Label, o.Description)})
			}
			elements = append(elements, button(fmt.Sprintf("%d. %s", qi+1, o.Label), core.Args{"revision": p.revision, "request": id, "question": qi, "option": oi}, "default"))
		}
	}
	elements = append(elements, button("提交选择", core.Args{"revision": p.revision, "request": id, "decision": "submit"}, "primary"), button("取消", core.Args{"revision": p.revision, "request": id, "decision": "deny"}, "default"))
	return DisplayCard("需要补充信息", "回答所有问题后提交。每位成员独立选择，以操作提示为准，卡片不显示个人勾选。\n自由回答：`/answer "+id+" 你的回答`。", elements...)
}
func (g *Gateway) settleApproval(id string, value core.Args, actor string) bool {
	g.approvalMu.Lock()
	p := g.approvals[id]
	if p == nil {
		g.approvalMu.Unlock()
		return false
	}
	state := "denied"
	if boolArg(value, "approved") {
		state = "approved"
	} else if stringArg(value, "reason") == "expired" {
		state = "expired"
	}
	res, e := g.store.DB.Exec("UPDATE requests SET state=?,actor=? WHERE id=? AND state='pending'", state, actor, id)
	durable := e == nil
	if durable {
		n, rowErr := res.RowsAffected()
		durable = rowErr == nil && n == 1
	}
	if !durable {
		// Only a committed approval can authorize execution. Abandon the
		// local request even when storage is unavailable; recovery expires
		// any still-pending row, and the old card loses its live binding.
		value = core.Args{"approved": false, "reason": "request could not be persisted; denied"}
		state = "unrecorded_denial"
	}
	delete(g.approvals, id)
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.detach != nil {
		p.detach()
	}
	p.cancel()
	cards := []string{}
	for id := range p.cards {
		cards = append(cards, id)
	}
	p.outcome <- value
	g.approvalMu.Unlock()
	g.audit(p.room.Key, "request_settled", core.Args{"id": id, "kind": p.kind, "state": state, "actor": actor})
	for _, mid := range cards {
		mid := mid
		g.task(func() {
			unlock := g.approvalUpdateLocks.lock(mid)
			defer unlock()
			title, text := "请求已结束", "未执行："+stringArg(value, "reason")
			if boolArg(value, "approved") {
				title = "已确认"
				text = "本次请求已确认，不再接受重复操作。"
			}
			_ = providerVoid(g.ctx, func() error { return g.transport.Update(g.ctx, mid, DisplayCard(title, text)) })
		})
	}
	return durable
}
func (g *Gateway) cancelApprovals(session string) {
	g.approvalMu.Lock()
	var ids []string
	for id, p := range g.approvals {
		if p.session == session {
			ids = append(ids, id)
		}
	}
	g.approvalMu.Unlock()
	for _, id := range ids {
		g.settleApproval(id, core.Args{"approved": false, "reason": "cancelled"}, "system")
	}
}
func (g *Gateway) closeApprovals() {
	g.approvalMu.Lock()
	var ids []string
	for id := range g.approvals {
		ids = append(ids, id)
	}
	g.approvalMu.Unlock()
	for _, id := range ids {
		g.settleApproval(id, core.Args{"approved": false, "reason": "shutdown"}, "system")
	}
}
func (g *Gateway) refreshQuestion(ctx context.Context, id string, p *approval, text string) (string, error) {
	g.approvalMu.Lock()
	if g.approvals[id] != p {
		g.approvalMu.Unlock()
		return "请求已结束或过期", nil
	}
	p.revision = core.NewID()
	card := g.redactArgs(approvalCard(id, p))
	revision := p.revision
	var cards []string
	for mid := range p.cards {
		cards = append(cards, mid)
	}
	g.approvalMu.Unlock()
	all := true
	for _, mid := range cards {
		unlock := g.approvalUpdateLocks.lock(mid)
		g.approvalMu.Lock()
		alive := g.approvals[id] == p && p.revision == revision
		g.approvalMu.Unlock()
		if alive {
			if e := providerVoid(ctx, func() error { return g.transport.Update(ctx, mid, card) }); e != nil {
				all = false
			}
		}
		unlock()
	}
	if !all {
		text += "；卡片刷新失败，请使用 /answer " + id + " 回答，或稍后重试。"
	}
	return text, nil
}

// Action validates the delivered card, current room ownership and current
// maintainer roster before it changes a durable approval decision.
func (g *Gateway) Action(ctx context.Context, a core.CardAction) (string, error) {
	if !g.beginCall() {
		return "", nil
	}
	defer g.wg.Done()
	id := stringArg(a.Value, "request")
	if id == "" {
		return "", nil
	}
	g.approvalMu.Lock()
	p := g.approvals[id]
	if p == nil {
		g.approvalMu.Unlock()
		var key string
		e := g.store.DB.QueryRow("SELECT room FROM requests WHERE id=?", id).Scan(&key)
		r, ok := RoomFromKey(g.config, key)
		if e == nil && ok && r.ChatID == a.ChatID && AuthorizedAuthor(r, a.UserID) {
			return "请求已结束或过期", nil
		}
		return "", nil
	}
	current, ok := RoomFromKey(g.config, p.room.Key)
	if !ok {
		g.approvalMu.Unlock()
		return "", nil
	}
	p.room = current
	if p.forwarded {
		if a.MessageID == "" || p.cards[a.MessageID] != a.UserID || !contains(current.Maintainers, a.UserID) {
			g.approvalMu.Unlock()
			return "", nil
		}
	} else {
		_, observed := p.cards[a.MessageID]
		if current.ChatID != a.ChatID || a.MessageID == "" || !observed || !AuthorizedAuthor(current, a.UserID) {
			g.approvalMu.Unlock()
			return "", nil
		}
	}
	if p.expires <= time.Now().UnixMilli() {
		g.approvalMu.Unlock()
		return "请求已结束或过期", nil
	}
	if p.kind == "permission" && !contains(current.Maintainers, a.UserID) {
		g.approvalMu.Unlock()
		return "仅维护者可以审批", nil
	}
	decision := stringArg(a.Value, "decision")
	if p.kind == "question" && stringArg(a.Value, "revision") != p.revision {
		g.approvalMu.Unlock()
		return "卡片已刷新，请使用最新卡片或 /answer 回答。", nil
	}
	if decision == "deny" {
		g.approvalMu.Unlock()
		if g.settleApproval(id, core.Args{"approved": false, "reason": "denied"}, a.UserID) {
			return "已拒绝", nil
		}
		return "请求已结束或过期", nil
	}
	if p.kind == "permission" {
		g.approvalMu.Unlock()
		if decision != "allow" {
			return "无效决定", nil
		}
		if g.settleApproval(id, core.Args{"approved": true}, a.UserID) {
			return "已批准一次", nil
		}
		return "请求已结束或过期", nil
	}
	if decision == "submit" {
		answers := map[string]string{}
		for qi, q := range p.questions {
			selected := p.selected[a.UserID][qi]
			if len(selected) == 0 {
				g.approvalMu.Unlock()
				return g.refreshQuestion(ctx, id, p, "请回答所有问题")
			}
			indices := []int{}
			for oi := range selected {
				indices = append(indices, oi)
			}
			sort.Ints(indices)
			labels := []string{}
			for _, oi := range indices {
				labels = append(labels, q.Options[oi].Label)
			}
			answers[q.Question] = strings.Join(labels, ", ")
		}
		g.approvalMu.Unlock()
		if g.settleApproval(id, core.Args{"approved": true, "answers": answers}, a.UserID) {
			return "已提交", nil
		}
		return "请求已结束或过期", nil
	}
	qi, qok := numberArg(a.Value, "question")
	oi, ook := numberArg(a.Value, "option")
	if !qok || !ook || qi >= int64(len(p.questions)) || oi >= int64(len(p.questions[qi].Options)) {
		g.approvalMu.Unlock()
		return g.refreshQuestion(ctx, id, p, "无效选项")
	}
	q := p.questions[qi]
	choices := p.selected[a.UserID]
	if choices == nil {
		choices = map[int]map[int]bool{}
	}
	selected := choices[int(qi)]
	if selected == nil || !q.MultiSelect {
		selected = map[int]bool{}
	}
	if selected[int(oi)] {
		delete(selected, int(oi))
	} else {
		selected[int(oi)] = true
	}
	choices[int(qi)] = selected
	p.selected[a.UserID] = choices
	indices := []int{}
	for index := range selected {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	labels := []string{}
	for _, index := range indices {
		labels = append(labels, q.Options[index].Label)
	}
	label := strings.Join(labels, ", ")
	if label == "" {
		label = "未选择"
	}
	g.approvalMu.Unlock()
	return g.refreshQuestion(ctx, id, p, "已记录你的选择："+cut(q.Question, 50)+" → "+cut(label, 80))
}
func (g *Gateway) answer(r core.Room, actor, id, text string) string {
	g.approvalMu.Lock()
	p := g.approvals[id]
	if p == nil || p.kind != "question" || p.room.Key != r.Key || p.expires <= time.Now().UnixMilli() {
		g.approvalMu.Unlock()
		return "问题不存在或已过期"
	}
	current, ok := RoomFromKey(g.config, r.Key)
	if !ok || !AuthorizedAuthor(current, actor) {
		g.approvalMu.Unlock()
		return ""
	}
	if strings.TrimSpace(text) == "" {
		g.approvalMu.Unlock()
		return "回答不能为空"
	}
	answers := map[string]string{}
	if len(p.questions) == 1 {
		answers[p.questions[0].Question] = text
	} else {
		if json.Unmarshal([]byte(text), &answers) != nil || len(answers) != len(p.questions) {
			g.approvalMu.Unlock()
			return "多题请使用卡片；或使用 JSON：{\"问题原文\":\"回答\"}"
		}
		for _, q := range p.questions {
			if _, ok := answers[q.Question]; !ok {
				g.approvalMu.Unlock()
				return "多题请使用卡片；或使用 JSON：{\"问题原文\":\"回答\"}"
			}
		}
	}
	g.approvalMu.Unlock()
	if g.settleApproval(id, core.Args{"approved": true, "answers": answers}, actor) {
		return "回答已提交"
	}
	return "问题不存在或已过期"
}

var _ = sql.ErrNoRows
