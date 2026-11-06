package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func (p *approval) live() bool {
	return p.ctx.Err() == nil && p.expires > time.Now().UnixMilli()
}

func (g *Gateway) bindApprovalCard(id string, p *approval, mid string, target core.Room) bool {
	g.approvalMu.Lock()
	defer g.approvalMu.Unlock()
	// Context cancellation invalidates the request immediately, even if its
	// asynchronous durable settlement has not removed the map entry yet.
	if g.approvals[id] != p || !p.live() {
		return false
	}
	owner := ""
	if p.forwarded {
		owner = target.DMUser
	}
	p.cards[mid] = owner
	return true
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
	resumeReplies := g.pauseReplies(r.Key, session, kind)
	defer resumeReplies()
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
	card := g.redactArgs(approvalCard(id, p, ""))
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
		g.notify(r, "这项操作需要项目维护者批准。审批卡正在发送，尚未确认送达；只发送操作摘要与参数，不会转发聊天历史。", nil)
	}
	g.task(func() {
		success := false
		var lastErr error
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
				lastErr = e
				continue
			}
			alive := g.bindApprovalCard(id, p, mid, target)
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
				if !success && p.forwarded {
					g.task(func() {
						_ = g.notice(waitCtx, r, "审批卡已确认送达至少一位项目维护者的私聊；本次操作仍需维护者批准。", nil)
					})
				}
				success = true
				if !p.forwarded {
					_ = g.store.RecordOutput(mid, r.Key, session)
				}
			}
		}
		if !success {
			reason := "card delivery failed"
			if errors.Is(lastErr, core.ErrCardTooLarge) {
				reason = "approval card exceeds the platform size limit; ask fewer questions or shorter option text"
			}
			g.settleApproval(id, core.Args{"approved": false, "reason": reason}, "system")
			if p.forwarded {
				g.notify(r, "未能确认审批卡送达项目维护者。本次请求已结束，操作未获批准。", nil)
			}
		}
	})
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
func approvalCard(id string, p *approval, actor string) core.Args {
	card := approvalCardText(id, p, actor, -1)
	if interactionCardFits(card) {
		return card
	}
	// Shrink model-supplied text until the card fits the platform budget. Option
	// entries, their values and the submit controls are preserved; only display
	// text is bounded so a long question cannot make approval impossible.
	lo, hi := 0, core.CardPayloadLimit
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if interactionCardFits(approvalCardText(id, p, actor, mid)) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return approvalCardText(id, p, actor, lo)
}

func interactionCardFits(card core.Args) bool {
	size, err := core.CardPayloadBytes(card)
	return err == nil && size <= core.CardPayloadLimit
}

func capInteractionText(text string, limit int) string {
	if limit < 0 || len(text) <= limit {
		return text
	}
	if limit == 0 {
		return "…"
	}
	return bytePrefix(text, limit) + "…"
}

func approvalCardText(id string, p *approval, actor string, limit int) core.Args {
	if p.kind == "permission" {
		return DisplayCard("需要维护者批准", capInteractionText(p.summary, limit)+"\n\n仅批准这一次操作，超时或停止后失效。\n请求：`"+id+"`", button("批准一次", core.Args{"request": id, "decision": "allow"}, "primary"), button("拒绝", core.Args{"request": id, "decision": "deny"}, "danger"))
	}
	intro := "每题标注单选/多选；群内每位成员独立选择，第一位提交者生效。"
	if p.room.DMUser != "" {
		intro = "每题标注单选/多选；选择后卡片会显示你的当前选择。"
	}
	if line := selectionSummary(p, actor); line != "" {
		intro += "\n\n" + line
	}
	intro = capInteractionText(intro, limit)
	elements := []any{}
	for qi, q := range p.questions {
		mode := "单选"
		if q.MultiSelect {
			mode = "多选"
		}
		header := capInteractionText(q.Header, limit)
		if header != "" {
			header += " · "
		}
		elements = append(elements, core.Args{"tag": "markdown", "content": fmt.Sprintf("**%d. %s%s**\n%s", qi+1, header, mode, capInteractionText(q.Question, limit))})
		for oi, o := range q.Options {
			selected := actor != "" && p.room.DMUser != "" && p.selected[actor][qi][oi]
			label := capInteractionText(o.Label, limit)
			style := "default"
			if selected {
				label = "✓ " + label
				style = "primary"
			}
			value := core.Args{"revision": p.revision, "request": id, "question": qi, "option": oi}
			if o.Description != "" && limit != 0 {
				elements = append(elements, optionRow(label, capInteractionText(o.Description, limit), value, style))
			} else {
				elements = append(elements, button(label, value, style))
			}
		}
	}
	elements = append(elements, actionRow(
		button("提交选择", core.Args{"revision": p.revision, "request": id, "decision": "submit"}, "primary"),
		button("取消", core.Args{"revision": p.revision, "request": id, "decision": "deny"}, "default"),
	))
	elements = append(elements, core.Args{"tag": "markdown", "text_size": "notation", "content": "卡片无法操作时：`/answer " + id + " 你的回答`。"})
	return DisplayCard("需要补充信息", intro, elements...)
}

// selectionSummary is only rendered into a private card. A shared group card
// must not disclose which options one member picked to the other members.
func selectionSummary(p *approval, actor string) string {
	if actor == "" || p.room.DMUser == "" {
		return ""
	}
	choices := p.selected[actor]
	lines := []string{}
	for qi, q := range p.questions {
		selected := choices[qi]
		if len(selected) == 0 {
			continue
		}
		indices := make([]int, 0, len(selected))
		for oi := range selected {
			indices = append(indices, oi)
		}
		sort.Ints(indices)
		labels := make([]string, 0, len(indices))
		for _, oi := range indices {
			labels = append(labels, q.Options[oi].Label)
		}
		lines = append(lines, fmt.Sprintf("%d. %s", qi+1, strings.Join(labels, ", ")))
	}
	if len(lines) == 0 {
		return ""
	}
	return "**当前选择**\n" + strings.Join(lines, "\n")
}
func (g *Gateway) settleApproval(id string, value core.Args, actor string) bool {
	g.approvalMu.Lock()
	p := g.approvals[id]
	if p == nil {
		g.approvalMu.Unlock()
		return false
	}
	accepted := true
	// Recheck after acquiring the settlement lock: a callback can have validated
	// the card just before its caller cancelled or the request expired.
	if p.ctx.Err() != nil {
		value = core.Args{"approved": false, "reason": "cancelled"}
		actor, accepted = "system", false
	} else if p.expires <= time.Now().UnixMilli() {
		value = core.Args{"approved": false, "reason": "expired"}
		actor, accepted = "system", false
	}
	state := "denied"
	if boolArg(value, "approved") {
		state = "approved"
	} else if stringArg(value, "reason") == "expired" {
		state = "expired"
	}
	writeCtx := context.Background()
	if state == "approved" {
		var cancelWrite context.CancelFunc
		writeCtx, cancelWrite = context.WithDeadline(p.ctx, time.UnixMilli(p.expires))
		defer cancelWrite()
	}
	durable := g.persistApproval(writeCtx, id, state, actor)
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
	return durable && accepted
}

func (g *Gateway) persistApproval(ctx context.Context, id, state, actor string) bool {
	// A cancelled positive decision must not sit in the connection queue and
	// later commit. The transaction also rolls back cancellation during UPDATE.
	tx, err := g.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE requests SET state=?,actor=? WHERE id=? AND state='pending'", state, actor, id)
	if err != nil {
		return false
	}
	n, err := res.RowsAffected()
	return err == nil && n == 1 && ctx.Err() == nil && tx.Commit() == nil
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

// Action keeps the string-only view used by tests and callers that do not need
// to return an updated card with the callback response.
func (g *Gateway) Action(ctx context.Context, a core.CardAction) (string, error) {
	response, err := g.ActionResponse(ctx, a)
	if err != nil {
		return "", err
	}
	return stringArg(argsOf(response["toast"]), "content"), nil
}

func actionToast(text string) core.Args {
	if text == "" {
		return core.Args{}
	}
	return core.Args{"toast": core.Args{"type": "info", "content": text}}
}

// ActionResponse validates the delivered card, current room ownership and
// current maintainer roster before it changes a durable approval decision. It
// returns the card callback payload, including an updated raw card when the
// caller's own selection needs immediate, persistent feedback.
func (g *Gateway) ActionResponse(ctx context.Context, a core.CardAction) (core.Args, error) {
	if !g.beginCall() {
		return core.Args{}, nil
	}
	defer g.wg.Done()
	if text, handled, err := g.quickAction(ctx, a); handled {
		return actionToast(text), err
	}
	id := stringArg(a.Value, "request")
	if id == "" {
		return core.Args{}, nil
	}
	g.approvalMu.Lock()
	p := g.approvals[id]
	if p == nil {
		g.approvalMu.Unlock()
		var key string
		e := g.store.DB.QueryRow("SELECT room FROM requests WHERE id=?", id).Scan(&key)
		r, ok := RoomFromKey(g.config, key)
		if e == nil && ok && r.ChatID == a.ChatID && AuthorizedAuthor(r, a.UserID) {
			return actionToast("请求已结束或过期"), nil
		}
		return core.Args{}, nil
	}
	current, ok := RoomFromKey(g.config, p.room.Key)
	if !ok {
		g.approvalMu.Unlock()
		return core.Args{}, nil
	}
	p.room = current
	if p.forwarded {
		if a.MessageID == "" || p.cards[a.MessageID] != a.UserID || !contains(current.Maintainers, a.UserID) {
			g.approvalMu.Unlock()
			return core.Args{}, nil
		}
	} else {
		_, observed := p.cards[a.MessageID]
		if current.ChatID != a.ChatID || a.MessageID == "" || !observed || !AuthorizedAuthor(current, a.UserID) {
			g.approvalMu.Unlock()
			return core.Args{}, nil
		}
	}
	if !p.live() {
		g.approvalMu.Unlock()
		return actionToast("请求已结束或过期"), nil
	}
	if p.kind == "permission" && !contains(current.Maintainers, a.UserID) {
		g.approvalMu.Unlock()
		return actionToast("仅维护者可以审批"), nil
	}
	decision := stringArg(a.Value, "decision")
	if decision == "deny" {
		g.approvalMu.Unlock()
		if g.settleApproval(id, core.Args{"approved": false, "reason": "denied"}, a.UserID) {
			return actionToast("已拒绝"), nil
		}
		return actionToast("请求已结束或过期"), nil
	}
	if p.kind == "permission" {
		g.approvalMu.Unlock()
		if decision != "allow" {
			return actionToast("无效决定"), nil
		}
		if g.settleApproval(id, core.Args{"approved": true}, a.UserID) {
			return actionToast("已批准一次"), nil
		}
		return actionToast("请求已结束或过期"), nil
	}
	if decision == "submit" {
		answers := map[string]string{}
		missing := []string{}
		for qi, q := range p.questions {
			selected := p.selected[a.UserID][qi]
			if len(selected) == 0 {
				missing = append(missing, fmt.Sprint(qi+1))
				continue
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
		if len(missing) > 0 {
			return actionToast("请先回答第 " + strings.Join(missing, "、") + " 题，再提交。"), nil
		}
		if g.settleApproval(id, core.Args{"approved": true, "answers": answers}, a.UserID) {
			return actionToast("已提交"), nil
		}
		return actionToast("请求已结束或过期"), nil
	}
	qi, qok := numberArg(a.Value, "question")
	oi, ook := numberArg(a.Value, "option")
	if !qok || !ook || qi >= int64(len(p.questions)) || oi >= int64(len(p.questions[qi].Options)) {
		g.approvalMu.Unlock()
		return actionToast("无效选项，请使用最新卡片或 /answer 回答。"), nil
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
	if q.MultiSelect && selected[int(oi)] {
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
	// A shared group card must not expose one member's choices. A private card
	// gets the updated raw card in the callback response so the client does not
	// revert to the pre-click snapshot after showing the toast.
	var responseCard core.Args
	if p.room.DMUser != "" {
		responseCard = g.redactArgs(approvalCard(id, p, a.UserID))
	}
	g.approvalMu.Unlock()
	hint := "点击「提交选择」生效"
	if q.MultiSelect {
		hint = "可继续选择，选好后提交"
	}
	reply := actionToast(fmt.Sprintf("第 %d 题已记录：%s（%s）", qi+1, cut(label, 80), hint))
	if responseCard != nil {
		reply["card"] = core.Args{"type": "raw", "data": responseCard}
	}
	return reply, nil
}
func (g *Gateway) answer(r core.Room, actor, id, text string) string {
	g.approvalMu.Lock()
	p := g.approvals[id]
	if p == nil || p.kind != "question" || p.room.Key != r.Key || !p.live() {
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
