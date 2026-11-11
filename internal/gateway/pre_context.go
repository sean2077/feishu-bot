package gateway

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/sean2077/feishu-bot/internal/core"
)

const (
	preContextSkipped = "skipped"
	preContextUsed    = "used"
	preContextFailed  = "failed"
)

type preContextResult struct {
	value  *core.PreContext
	status string
	reason string
	ms     int64
}

type preContextRun struct {
	delivery string
	ctx      context.Context
	cancel   context.CancelFunc
	result   chan preContextResult
	started  time.Time
}

// wait returns once the retrieval finishes or its configured deadline passes.
// The action goroutine remains tracked by the gateway and observes the same
// context, so a slow resource cleanup cannot hold up the user's input.
func (p *preContextRun) wait() preContextResult {
	select {
	case result := <-p.result:
		p.cancel()
		return result
	default:
	}
	select {
	case result := <-p.result:
		p.cancel()
		return result
	case <-p.ctx.Done():
		select {
		case result := <-p.result:
			p.cancel()
			return result
		default:
		}
		p.cancel()
		return preContextResult{status: preContextFailed, reason: "timeout", ms: time.Since(p.started).Milliseconds()}
	}
}

func (p *preContextRun) stop() {
	if p != nil {
		p.cancel()
	}
}

func preContextTriggerText(d core.Delivery) string {
	// An image-only message carries a rendered placeholder, not a user query.
	// A post/image batch with a caption keeps ImageOnly false and uses that
	// caption; the first member's text is the only one passed.
	if d.Message.ImageOnly {
		return ""
	}
	return strings.TrimSpace(d.Message.Text)
}

// normalizePreContextText folds every whitespace or control run into one ASCII
// space and trims both ends. The action parameter policy can therefore reject
// control characters without rejecting multi-line human questions.
func normalizePreContextText(text string) string {
	var b strings.Builder
	space := false
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			if b.Len() > 0 {
				space = true
			}
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncatePreContext(text string, max int) (string, bool) {
	runes := []rune(strings.ToValidUTF8(text, "�"))
	if max <= 0 || len(runes) <= max {
		return string(runes), false
	}
	return string(runes[:max]), true
}

func preContextRequestContext(d core.Delivery) *core.RequestContext {
	m := d.Message
	current := &core.RequestContext{
		ChatID:       m.ChatID,
		ChatType:     m.ChatType,
		ThreadID:     m.ThreadID,
		MessageID:    m.ID,
		SenderOpenID: m.SenderID,
		SenderName:   m.SenderName,
	}
	if m.SenderID != "" {
		current.Senders = []string{m.SenderID}
	}
	if m.ID != "" {
		current.MessageIDs = []string{m.ID}
	}
	return current
}

func preContextErrorReason(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "operation timed out"), strings.Contains(message, "timed out"):
		return "timeout"
	case strings.Contains(message, "invalid parameter"), strings.Contains(message, "parameter does not match policy"),
		strings.Contains(message, "invalid option"), strings.Contains(message, "unknown parameter"):
		return "parameter"
	case errors.Is(err, context.Canceled), strings.Contains(message, "operation cancelled"):
		return "cancelled"
	}
	return "execution"
}

func (g *Gateway) runPreContext(ctx context.Context, r core.Room, p core.Project, d core.Delivery) preContextResult {
	started := time.Now()
	pc := p.PreContext
	if pc == nil {
		return preContextResult{status: preContextSkipped}
	}
	value := normalizePreContextText(preContextTriggerText(d))
	if value == "" {
		return preContextResult{status: preContextFailed, reason: "parameter"}
	}
	var spec *core.ActionSpec
	for i := range p.Actions {
		if p.Actions[i].ID == pc.Action {
			spec = &p.Actions[i]
			break
		}
	}
	if spec == nil {
		return preContextResult{status: preContextFailed, reason: "configuration"}
	}
	if !spec.ReadOnly {
		return preContextResult{status: preContextFailed, reason: "configuration"}
	}
	param, ok := spec.Parameters[pc.Parameter]
	if !ok || param.WorkspaceFile {
		return preContextResult{status: preContextFailed, reason: "configuration"}
	}
	if runes := []rune(value); len(runes) > param.MaxLength {
		value = string(runes[:param.MaxLength])
	}
	input := core.Args{pc.Parameter: value}
	result, err := g.executeActionWithPolicy(ctx, r, d.SessionID, p, pc.Action, input, core.Workspace{}, preContextRequestContext(d), pc.Parameter)
	ms := time.Since(started).Milliseconds()
	if err != nil {
		return preContextResult{status: preContextFailed, reason: preContextErrorReason(ctx, err), ms: ms}
	}
	code, _ := numberArg(result, "code")
	if !boolArg(result, "ok") || code != 0 {
		return preContextResult{status: preContextFailed, reason: "exit", ms: ms}
	}
	output := strings.TrimSpace(stringArg(result, "output"))
	if output == "" {
		return preContextResult{status: preContextFailed, reason: "empty", ms: ms}
	}
	text, truncated := truncatePreContext(output, pc.MaxChars)
	return preContextResult{value: &core.PreContext{Source: pc.Action, Text: text, Truncated: truncated}, status: preContextUsed, ms: ms}
}

func (g *Gateway) startPreContext(base context.Context, r core.Room, p core.Project, d core.Delivery) *preContextRun {
	if p.PreContext == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(base, time.Duration(p.PreContext.TimeoutMS)*time.Millisecond)
	run := &preContextRun{delivery: d.UUID, ctx: ctx, cancel: cancel, result: make(chan preContextResult, 1), started: time.Now()}
	if !g.task(func() { run.result <- g.runPreContext(ctx, r, p, d) }) {
		cancel()
		return nil
	}
	return run
}

// preContextEligible decides whether the first pending input opens a new turn
// and can therefore start retrieval before the native process is launched.
func preContextEligible(s *slot, p core.Project, d core.Delivery) bool {
	if p.PreContext == nil || d.SessionID != s.session.ID || d.Command != nil || d.SupplementTo != "" || preContextTriggerText(d) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn == nil && s.compacting == "" && !s.stopUnconfirmed && len(s.deliveries) == 0
}

// preContextForTurnStart handles the fallback when the prestarted delivery was
// not the input that actually opened the turn.
func (g *Gateway) preContextForTurnStart(s *slot, r core.Room, p core.Project, d core.Delivery) preContextResult {
	if p.PreContext == nil || d.Command != nil || d.SupplementTo != "" || preContextTriggerText(d) == "" {
		return preContextResult{status: preContextSkipped}
	}
	s.mu.Lock()
	open := s.turn == nil && s.compacting == "" && !s.stopUnconfirmed && len(s.deliveries) == 0
	s.mu.Unlock()
	if !open {
		return preContextResult{status: preContextSkipped}
	}
	run := g.startPreContext(s.ctx, r, p, d)
	if run == nil {
		return preContextResult{status: preContextSkipped}
	}
	return run.wait()
}

func (g *Gateway) auditPreContextFailure(r core.Room, p core.Project, result preContextResult) {
	if result.status != preContextFailed || p.PreContext == nil {
		return
	}
	g.audit(r.Key, "pre_context_failed", core.Args{
		"project":    p.ID,
		"action":     p.PreContext.Action,
		"reason":     result.reason,
		"durationMs": result.ms,
	})
}
