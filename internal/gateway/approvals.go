package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
	approvers                              []string
	owners                                 []string
	forbidSelfApproval                     bool
	designated                             bool
	requester                              *core.RequestContext
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
func (g *Gateway) ask(ctx context.Context, r core.Room, session, name string, input core.Args, summary string, questions []question, policy approvalPolicy) (core.Args, error) {
	if ctx.Err() != nil {
		return core.Args{"approved": false, "reason": "cancelled"}, nil
	}
	kind := "permission"
	if questions != nil {
		kind = "question"
	}
	approvers := policy.approvers
	if kind == "permission" && len(approvers) == 0 {
		approvers = r.Maintainers
	}
	forwarded := kind == "permission" && r.DMUser != "" && !contains(approvers, r.DMUser)
	resumeReplies := g.pauseReplies(r.Key, session, kind)
	defer resumeReplies()
	id := core.NewID()
	expires := time.Now().UnixMilli() + g.config.Limits.ApprovalMS
	waitCtx, cancel := context.WithCancel(ctx)
	p := &approval{room: r, session: session, kind: kind, name: name, summary: summary, revision: core.NewID(), expires: expires, cards: map[string]string{}, forwarded: forwarded, questions: questions, approvers: approvers, owners: policy.owners, forbidSelfApproval: policy.forbidSelfApproval, designated: policy.designated, requester: policy.requestContext, outcome: make(chan core.Args, 1), ctx: waitCtx, cancel: cancel}
	g.approvalMu.Lock()
	if len(g.approvals) >= 256 {
		g.approvalMu.Unlock()
		cancel()
		return core.Args{"approved": false, "reason": "too many pending requests"}, nil
	}
	var requester, contextJSON any
	if policy.requestContext != nil {
		if policy.requestContext.SenderOpenID != "" {
			requester = policy.requestContext.SenderOpenID
		}
		if data, marshalErr := json.Marshal(policy.requestContext); marshalErr == nil {
			contextJSON = string(data)
		}
	}
	_, e := g.store.DB.Exec("INSERT INTO requests(id,room,session,kind,digest,state,expires,requester,request_context) VALUES(?,?,?,?,?,'pending',?,?,?)", id, r.Key, session, kind, core.Hash(jsonText(core.Args{"name": name, "input": input})), expires, requester, contextJSON)
	if e != nil {
		g.approvalMu.Unlock()
		cancel()
		return nil, e
	}
	g.approvals[id] = p
	if kind == "permission" && policy.tainted {
		g.audit(r.Key, "tainted_turn_prompt", core.Args{
			"room":    r.Key,
			"session": session,
			"tool":    name,
			"request": id,
			"targets": append([]string(nil), policy.taintTargets...),
		})
	}
	p.timer = time.AfterFunc(time.Duration(g.config.Limits.ApprovalMS)*time.Millisecond, func() { g.settleApproval(id, core.Args{"approved": false, "reason": "expired"}, "system") })
	p.detach = context.AfterFunc(ctx, func() { g.settleApproval(id, core.Args{"approved": false, "reason": "cancelled"}, "system") })
	card := g.redactArgs(approvalCard(id, p))
	g.approvalMu.Unlock()
	targets := []core.Room{r}
	if p.forwarded {
		targets = nil
		for _, id := range approvers {
			target := r
			target.ChatID = id
			target.DMUser = id
			targets = append(targets, target)
		}
		g.notify(r, "这项操作需要"+p.approverLabel()+"批准。审批卡正在发送，尚未确认送达；只发送操作摘要与参数，不会转发聊天历史。", nil)
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
						_ = g.notice(waitCtx, r, "审批卡已确认送达至少一位"+p.approverLabel()+"的私聊；本次操作仍需"+p.approverLabel()+"批准。", nil)
					})
				}
				success = true
				if !p.forwarded {
					_ = g.store.RecordOutput(mid, r.Key, session)
				}
			}
		}
		if !success {
			// Cancellation or expiry already settles the request through its own
			// callback; only an actual delivery failure reports undelivered cards.
			ended := waitCtx.Err() != nil
			reason := "card delivery failed"
			if errors.Is(lastErr, core.ErrCardTooLarge) {
				reason = "approval card exceeds the platform size limit; ask fewer questions or shorter option text"
			}
			g.settleApproval(id, core.Args{"approved": false, "reason": reason}, "system")
			if p.forwarded && !ended {
				g.notify(r, "未能确认审批卡送达"+p.approverLabel()+"。本次请求已结束，操作未获批准。", nil)
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
func approvalCard(id string, p *approval) core.Args {
	card := approvalCardText(id, p, -1)
	if interactionCardFits(card) {
		return card
	}
	// Shrink model-supplied text until the card fits the platform budget. Option
	// entries, their values and the submit controls are preserved; only display
	// text is bounded so a long question cannot make approval impossible.
	lo, hi := 0, core.CardPayloadLimit
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if interactionCardFits(approvalCardText(id, p, mid)) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return approvalCardText(id, p, lo)
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

const questionAnswerLimit = 1000

func approvalCardText(id string, p *approval, limit int) core.Args {
	if p.kind == "permission" {
		title := "需要维护者批准"
		if p.designated {
			title = "需要指定审批人批准"
		}
		return DisplayCard(title, capInteractionText(p.summary, limit)+"\n\n仅批准这一次操作，超时或停止后失效。\n请求：`"+id+"`", button("批准一次", core.Args{"request": id, "decision": "allow"}, "primary"), button("拒绝", core.Args{"request": id, "decision": "deny"}, "danger"))
	}
	intro := "每题可选择选项或填写自定义回答；非空自定义回答优先，留空则使用选项。群内每位成员独立填写，第一位完整提交者生效。"
	if p.room.DMUser != "" {
		intro = "每题可选择选项或填写自定义回答；非空自定义回答优先，留空则使用选项。全部填写后一次提交。"
	}
	intro = capInteractionText(intro, limit)
	elements := []any{}
	for qi, q := range p.questions {
		mode := "单选"
		if q.MultiSelect {
			mode = "多选"
		}
		if len(q.Options) == 0 {
			mode = "自定义回答"
		}
		header := capInteractionText(q.Header, limit)
		if header != "" {
			header += " · "
		}
		elements = append(elements, core.Args{"tag": "markdown", "content": fmt.Sprintf("**%d. %s%s**\n%s", qi+1, header, mode, capInteractionText(q.Question, limit))})
		if len(q.Options) > 0 {
			options := make([]any, 0, len(q.Options))
			for oi, o := range q.Options {
				label := cut(capInteractionText(o.Label, limit), 80)
				options = append(options, core.Args{"text": core.Args{"tag": "plain_text", "content": label}, "value": strconv.Itoa(oi)})
				if o.Description != "" && limit != 0 {
					elements = append(elements, core.Args{"tag": "markdown", "content": label + " · " + capInteractionText(o.Description, limit)})
				}
			}
			tag := "select_static"
			if q.MultiSelect {
				tag = "multi_select_static"
			}
			elements = append(elements, core.Args{"tag": tag, "name": fmt.Sprintf("question_%d", qi), "width": "fill", "options": options, "placeholder": core.Args{"tag": "plain_text", "content": "选择选项，或填写下方自定义回答"}})
		}
		elements = append(elements, core.Args{"tag": "input", "name": fmt.Sprintf("custom_%d", qi), "input_type": "multiline_text", "rows": 2, "width": "fill", "max_length": questionAnswerLimit, "placeholder": core.Args{"tag": "plain_text", "content": "自定义回答（选填；填写后优先于选项）"}})
	}
	submit := button("提交回答", core.Args{"revision": p.revision, "session": p.session, "request": id, "decision": "submit"}, "primary")
	submit["name"], submit["form_action_type"] = "question_submit", "submit"
	cancel := button("取消", core.Args{"revision": p.revision, "session": p.session, "request": id, "decision": "deny"}, "default")
	cancel["name"], cancel["form_action_type"] = "question_cancel", "submit"
	elements = append(elements, actionRow(submit, cancel))
	form := core.Args{"tag": "form", "name": "question_form", "elements": elements}
	fallback := core.Args{"tag": "markdown", "text_size": "notation", "content": "卡片无法操作时：`/answer " + id + " 你的回答`。多题请使用 JSON：{\"问题原文\":\"回答\"}。"}
	return DisplayCard("需要补充信息", intro, form, fallback)
}

// Form fields are local to the authenticated submitter. Never merge them with
// another callback or interpret custom text as a gateway command or permission.
func questionFormAnswers(questions []question, form core.Args) (map[string]string, string) {
	answers := map[string]string{}
	missing := []string{}
	for qi, q := range questions {
		custom, ok := form[fmt.Sprintf("custom_%d", qi)].(string)
		if value := form[fmt.Sprintf("custom_%d", qi)]; value != nil && !ok {
			return nil, fmt.Sprintf("第 %d 题的自定义回答格式无效。", qi+1)
		}
		if !utf8.ValidString(custom) || utf8.RuneCountInString(custom) > questionAnswerLimit {
			return nil, fmt.Sprintf("第 %d 题的自定义回答最多 %d 字。", qi+1, questionAnswerLimit)
		}
		if custom = strings.TrimSpace(custom); custom != "" {
			answers[q.Question] = custom
			continue
		}
		indices, valid := questionFormChoices(form[fmt.Sprintf("question_%d", qi)], q)
		if !valid {
			return nil, fmt.Sprintf("第 %d 题的选项无效，请使用最新卡片。", qi+1)
		}
		if len(indices) == 0 {
			missing = append(missing, strconv.Itoa(qi+1))
			continue
		}
		labels := make([]string, 0, len(indices))
		for _, oi := range indices {
			labels = append(labels, q.Options[oi].Label)
		}
		answers[q.Question] = strings.Join(labels, ", ")
	}
	if len(missing) > 0 {
		return nil, "请先回答第 " + strings.Join(missing, "、") + " 题，再提交。"
	}
	return answers, ""
}

func questionFormChoices(raw any, q question) ([]int, bool) {
	if raw == nil || raw == "" {
		return nil, true
	}
	var values []string
	if !q.MultiSelect {
		value, ok := raw.(string)
		if !ok {
			return nil, false
		}
		values = []string{value}
	} else {
		switch value := raw.(type) {
		case []string:
			values = value
		case []any:
			for _, item := range value {
				s, ok := item.(string)
				if !ok {
					return nil, false
				}
				values = append(values, s)
			}
		default:
			return nil, false
		}
	}
	indices := make([]int, 0, len(values))
	seen := map[int]bool{}
	for _, value := range values {
		oi, err := strconv.Atoi(value)
		if err != nil || oi < 0 || oi >= len(q.Options) || strconv.Itoa(oi) != value || seen[oi] {
			return nil, false
		}
		seen[oi] = true
		indices = append(indices, oi)
	}
	sort.Ints(indices)
	return indices, true
}
func (g *Gateway) settleApproval(id string, value core.Args, actor string) bool {
	accepted, _ := g.settleApprovalContext(context.Background(), id, value, actor)
	return accepted
}

func (g *Gateway) settleApprovalContext(ctx context.Context, id string, value core.Args, actor string) (bool, error) {
	if !lockWithin(ctx, &g.approvalMu) {
		return false, ctx.Err()
	}
	p := g.approvals[id]
	if p == nil {
		g.approvalMu.Unlock()
		return false, nil
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
	writeCtx := ctx
	if state == "approved" {
		deadline := time.UnixMilli(p.expires)
		if callbackDeadline, ok := ctx.Deadline(); ok && callbackDeadline.Before(deadline) {
			deadline = callbackDeadline
		}
		var cancelWrite context.CancelFunc
		// Keep the native request as the parent so stop/expiry cancellation
		// still reaches its transaction directly. The earlier callback deadline
		// also applies, including manual cancellation while storage is queued.
		writeCtx, cancelWrite = context.WithDeadline(p.ctx, deadline)
		defer cancelWrite()
		stopCancel := context.AfterFunc(ctx, cancelWrite)
		defer stopCancel()
		if ctx.Err() != nil {
			cancelWrite()
		}
	}
	durable := g.persistApproval(writeCtx, id, state, actor)
	if !durable && ctx.Err() != nil {
		// The callback was never accepted. Keep the request and its card live
		// so a later click can try again; its own expiry/cancellation still
		// settles independently. A committed decision must continue below even
		// if the callback expires immediately after Commit.
		g.approvalMu.Unlock()
		return false, ctx.Err()
	}
	fallback := ""
	if !durable && state == "approved" && writeCtx.Err() != nil {
		// The approval lost its window while waiting for storage. The native
		// request must not wait again, so record the negative outcome after
		// settlement instead of leaving the row pending until restart.
		fallback = "expired"
		if p.ctx.Err() != nil {
			fallback = "denied"
		}
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
	value["actor"] = actor
	value["request"] = id
	value["settledAt"] = time.Now().UnixMilli()
	p.outcome <- value
	g.approvalMu.Unlock()
	if fallback != "" {
		g.task(func() { _ = g.persistApproval(context.Background(), id, fallback, "system") })
	}
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
	return durable && accepted, nil
}

func (g *Gateway) persistApproval(ctx context.Context, id, state, actor string) bool {
	// A cancelled positive decision must not sit in the connection queue and
	// later commit. The transaction also rolls back cancellation during UPDATE.
	tx, err := g.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	// Check the selected session inside the same transaction as the decision.
	// A callback validated just before /new or /resume must not answer an old
	// native request after its session has been archived.
	res, err := tx.ExecContext(ctx, `UPDATE requests SET state=?,actor=? WHERE id=? AND state='pending'
AND (?!='approved' OR (expires>? AND EXISTS (
SELECT 1 FROM sessions s WHERE s.id=requests.session AND s.room=requests.room AND s.state='open'
)))`, state, actor, id, state, time.Now().UnixMilli())
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
// current maintainer roster before it changes a durable approval decision.
// Question forms submit one actor's complete local draft in a single callback;
// rejected submissions leave that draft untouched on the client.
func (g *Gateway) ActionResponse(ctx context.Context, a core.CardAction) (core.Args, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !g.beginCallback(ctx) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
	if !lockWithin(ctx, &g.approvalMu) {
		return nil, ctx.Err()
	}
	p := g.approvals[id]
	if p == nil {
		g.approvalMu.Unlock()
		var key string
		e := g.store.DB.QueryRowContext(ctx, "SELECT room FROM requests WHERE id=?", id).Scan(&key)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
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
		if a.MessageID == "" || p.cards[a.MessageID] != a.UserID {
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
	if p.kind == "question" && (p.revision == "" || stringArg(a.Value, "revision") != p.revision || stringArg(a.Value, "session") != p.session) {
		g.approvalMu.Unlock()
		return actionToast("卡片已失效，请使用最新追问卡片或 /answer 回答。"), nil
	}
	decision := stringArg(a.Value, "decision")
	if p.kind == "permission" {
		allowed, toast, reason := p.decisionAllowed(a.UserID, decision == "deny")
		if !allowed {
			g.approvalMu.Unlock()
			if reason != "" {
				g.audit(p.room.Key, "approval_self_rejected", core.Args{"request": id, "user": a.UserID, "reason": reason})
			}
			return actionToast(toast), nil
		}
	}
	if decision == "deny" {
		g.approvalMu.Unlock()
		accepted, err := g.settleApprovalContext(ctx, id, core.Args{"approved": false, "reason": "denied"}, a.UserID)
		if err != nil {
			return nil, err
		}
		if accepted {
			return actionToast("已拒绝"), nil
		}
		return actionToast("请求已结束或过期"), nil
	}
	if p.kind == "permission" {
		g.approvalMu.Unlock()
		if decision != "allow" {
			return actionToast("无效决定"), nil
		}
		accepted, err := g.settleApprovalContext(ctx, id, core.Args{"approved": true}, a.UserID)
		if err != nil {
			return nil, err
		}
		if accepted {
			return actionToast("已批准一次"), nil
		}
		return actionToast("请求已结束或过期"), nil
	}
	if decision != "submit" {
		g.approvalMu.Unlock()
		return actionToast("无效决定，请使用最新追问卡片或 /answer 回答。"), nil
	}
	answers, issue := questionFormAnswers(p.questions, a.FormValue)
	g.approvalMu.Unlock()
	if issue != "" {
		return actionToast(issue), nil
	}
	accepted, err := g.settleApprovalContext(ctx, id, core.Args{"approved": true, "answers": answers}, a.UserID)
	if err != nil {
		return nil, err
	}
	if accepted {
		return actionToast("已提交"), nil
	}
	return actionToast("请求已结束或过期"), nil
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
