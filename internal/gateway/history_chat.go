package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const (
	historyScopeTopic = "topic"
	historyScopeChat  = "chat"

	maxHistoryExpandedThreads  = 20
	maxHistoryThreadReplies    = 10
	historyExpandWindowDays    = 3
	maxHistoryDiscoveryPages   = 3
	maxHistoryDiscoveryThreads = 50
	historyExpandConcurrency   = 4
)

type historyTarget struct {
	chatID        string
	room          core.Room
	source        core.Room
	session       string
	currentThread string
}

type fetchedHistoryThread struct {
	root      core.Message
	replies   []core.Message
	latest    int64
	available int
	err       error
}

func (g *Gateway) validateChatHistoryScope(source core.Room) (core.Room, error) {
	if source.DMUser != "" {
		return core.Room{}, errors.New("chat history scope is not enabled for this group")
	}
	base, ok := g.config.RoomFor(source.ChatID, "")
	if !ok || base.SessionScope != core.SessionScopeThread || base.TopicHistory != "chat" {
		return core.Room{}, errors.New("chat history scope is not enabled for this group")
	}
	return base, nil
}

func (g *Gateway) chatHistoryTarget(ctx context.Context, source core.Room, session string) (historyTarget, error) {
	base, err := g.validateChatHistoryScope(source)
	if err != nil {
		return historyTarget{}, err
	}
	currentThread := ""
	if source.Thread != "" {
		currentThread, err = g.topicThread(ctx, source)
		if err != nil {
			return historyTarget{}, err
		}
	}
	return historyTarget{chatID: source.ChatID, room: base, source: source, session: session, currentThread: currentThread}, nil
}

func parseHistoryLimit(input core.Args, q *core.HistoryQuery) error {
	if _, exists := input["limit"]; exists {
		v, ok := numberArg(input, "limit")
		if !ok || v < 1 || v > 50 {
			return errors.New("limit must be 1..50")
		}
		q.Limit = int(v)
	}
	return nil
}

func parseHistoryTimes(input core.Args, q *core.HistoryQuery) error {
	for _, key := range []string{"startTime", "endTime"} {
		if _, exists := input[key]; !exists {
			continue
		}
		v, ok := numberArg(input, key)
		if !ok {
			return fmt.Errorf("%s must be a Unix timestamp in seconds", key)
		}
		if key == "startTime" {
			q.StartTime = v
		} else {
			q.EndTime = v
		}
	}
	return nil
}

func parseHistoryThread(input core.Args, q *core.HistoryQuery) error {
	if value, exists := input["threadId"]; exists {
		s, ok := value.(string)
		if !ok || len(s) > 200 {
			return errors.New("invalid thread ID")
		}
		q.ThreadID = s
	}
	return nil
}

func (g *Gateway) parseChatHistoryQuery(source core.Room, input core.Args) (core.HistoryQuery, error) {
	q := core.HistoryQuery{Limit: 20, Scope: historyScopeChat, Chat: source.ChatID}
	if err := parseHistoryLimit(input, &q); err != nil {
		return q, err
	}
	if err := parseHistoryTimes(input, &q); err != nil {
		return q, err
	}
	if err := parseHistoryThread(input, &q); err != nil {
		return q, err
	}
	if raw, has := input["expandThreads"]; has {
		value, ok := raw.(bool)
		if !ok {
			return q, errors.New("expandThreads must be a boolean")
		}
		q.ExpandThreads = value
	}
	if q.ThreadID != "" {
		if q.ExpandThreads {
			return q, errors.New("expandThreads cannot be combined with threadId")
		}
		if q.StartTime > 0 || q.EndTime > 0 {
			return q, errors.New("startTime and endTime are not supported for thread history; page with the cursor instead")
		}
	} else {
		if q.EndTime == 0 {
			q.EndTime = time.Now().Unix()
		}
		if q.StartTime > q.EndTime {
			return q, errors.New("invalid history time range")
		}
	}
	if _, err := g.validateChatHistoryScope(source); err != nil {
		return q, err
	}
	return q, nil
}

func (g *Gateway) historyChat(ctx context.Context, source core.Room, session string, q core.HistoryQuery) (core.Args, error) {
	target, err := g.chatHistoryTarget(ctx, source, session)
	if err != nil {
		return nil, err
	}
	if q.Chat != "" && q.Chat != target.chatID {
		return nil, errors.New("history cursor belongs to another chat")
	}
	q.Scope, q.Chat = historyScopeChat, target.chatID
	if q.ThreadID != "" && q.ExpandThreads {
		return nil, errors.New("expandThreads cannot be combined with threadId")
	}
	if q.ThreadID != "" && (q.StartTime > 0 || q.EndTime > 0) {
		return nil, errors.New("startTime and endTime are not supported for thread history; page with the cursor instead")
	}
	if q.ThreadID != "" {
		observed, err := g.store.HasContextThread(source.Key, q.ThreadID)
		if err != nil {
			return nil, err
		}
		if !observed {
			return nil, errors.New("thread must first be observed in this conversation")
		}
		page, err := provider(ctx, func() (core.HistoryPage, error) { return g.transport.History(ctx, target.room, q) })
		if err != nil {
			return nil, err
		}
		if !historyThreadBelongs(target, page.Messages) {
			return nil, errors.New("thread does not belong to this group")
		}
		messages, err := g.chatHistoryMessages(ctx, target, q, page.Messages)
		if err != nil {
			return nil, err
		}
		threads := []string{q.ThreadID}
		g.auditChatHistory(target, threads, len(messages))
		return g.historyChatResult(target, q, messages, page.NextPageToken, nil, "scope=chat read one observed topic in this group; only authorized authors and this bot are returned."), nil
	}
	if q.ExpandThreads {
		return g.expandChatHistory(ctx, target, q)
	}
	page, err := provider(ctx, func() (core.HistoryPage, error) { return g.transport.History(ctx, target.room, q) })
	if err != nil {
		return nil, err
	}
	messages, err := g.chatHistoryMessages(ctx, target, q, page.Messages)
	if err != nil {
		return nil, err
	}
	threads := historyThreadIDs(messages, "")
	g.auditChatHistory(target, threads, len(messages))
	return g.historyChatResult(target, q, messages, page.NextPageToken, nil, "scope=chat reads the group timeline; only authorized authors and this bot are returned. This is one API page, not the full history."), nil
}

func historyThreadBelongs(target historyTarget, messages []core.Message) bool {
	for _, m := range messages {
		if m.ChatID == target.chatID {
			return true
		}
	}
	return len(messages) == 0
}

func (g *Gateway) chatHistoryMessages(ctx context.Context, target historyTarget, q core.HistoryQuery, raw []core.Message) ([]core.Message, error) {
	selected := raw[:min(q.Limit, len(raw))]
	readable := make([]core.Message, 0, len(selected))
	for _, m := range selected {
		if !Readable(target.room, m) {
			continue
		}
		m.Background = target.currentThread == "" || m.ThreadID != target.currentThread
		readable = append(readable, m)
	}
	if err := g.backfillHistoryTexts(ctx, target, readable); err != nil {
		return nil, err
	}
	remaining := min(g.config.Limits.TextChars*2, 40000)
	messages := make([]core.Message, 0, len(readable))
	for _, m := range readable {
		m.Text = cut(m.Text, min(g.config.Limits.TextChars, remaining))
		remaining -= len([]rune(m.Text))
		if len(m.Resources) > 12 {
			m.Resources = m.Resources[:12]
		}
		messages = append(messages, m)
	}
	if err := g.store.RememberContext(target.source.Key, messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func (g *Gateway) expandChatHistory(ctx context.Context, target historyTarget, q core.HistoryQuery) (core.Args, error) {
	if q.EndTime == 0 {
		q.EndTime = time.Now().Unix()
	}
	if q.StartTime > q.EndTime {
		return nil, errors.New("invalid history time range")
	}
	discoveryStart := q.EndTime - historyExpandWindowDays*86400
	if q.StartTime > 0 && q.StartTime < discoveryStart {
		discoveryStart = q.StartTime
	}
	token := q.PageToken
	seenMessages := map[string]bool{}
	seenThreads := map[string]bool{}
	var all []core.Message
	var roots []core.Message
	for page := 0; page < maxHistoryDiscoveryPages && len(seenThreads) < maxHistoryDiscoveryThreads; page++ {
		query := q
		query.ThreadID, query.ExpandThreads, query.PageToken = "", false, token
		query.StartTime, query.EndTime, query.Limit = discoveryStart, q.EndTime, 50
		result, err := provider(ctx, func() (core.HistoryPage, error) { return g.transport.History(ctx, target.room, query) })
		if err != nil {
			return nil, err
		}
		for _, m := range result.Messages {
			if m.ID != "" && seenMessages[m.ID] {
				continue
			}
			if m.ID != "" {
				seenMessages[m.ID] = true
			}
			all = append(all, m)
			if m.ThreadID != "" && !seenThreads[m.ThreadID] && len(seenThreads) < maxHistoryDiscoveryThreads {
				seenThreads[m.ThreadID] = true
				roots = append(roots, m)
			}
		}
		token = result.NextPageToken
		if token == "" {
			break
		}
	}

	base := []core.Message{}
	for _, m := range all {
		if m.ThreadID != "" || !historyInRange(m.Time, q.StartTime, q.EndTime) || !Readable(target.room, m) {
			continue
		}
		m.Background = target.currentThread == "" || m.ThreadID != target.currentThread
		base = append(base, m)
	}

	threads := make([]fetchedHistoryThread, len(roots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, historyExpandConcurrency)
	for i, root := range roots {
		wg.Add(1)
		go func(i int, root core.Message) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			query := core.HistoryQuery{Limit: maxHistoryThreadReplies + 1, ThreadID: root.ThreadID, Scope: historyScopeChat, Chat: target.chatID}
			page, err := provider(ctx, func() (core.HistoryPage, error) { return g.transport.History(ctx, target.room, query) })
			if err != nil {
				threads[i] = fetchedHistoryThread{root: root, err: err}
				return
			}
			replies := []core.Message{}
			for _, m := range page.Messages {
				if m.ID == root.ID || (m.ThreadID != "" && m.ThreadID != root.ThreadID) || !Readable(target.room, m) || !historyInRange(m.Time, q.StartTime, q.EndTime) {
					continue
				}
				m.Background = target.currentThread == "" || m.ThreadID != target.currentThread
				replies = append(replies, m)
			}
			sort.SliceStable(replies, func(a, b int) bool {
				if replies[a].Time == replies[b].Time {
					return replies[a].ID > replies[b].ID
				}
				return replies[a].Time > replies[b].Time
			})
			latest := root.Time
			for _, reply := range replies {
				if reply.Time > latest {
					latest = reply.Time
				}
			}
			threads[i] = fetchedHistoryThread{root: root, replies: replies, latest: latest, available: len(replies)}
		}(i, root)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	failedThreads := 0
	var firstThreadErr error
	for _, thread := range threads {
		if thread.err != nil {
			failedThreads++
			if firstThreadErr == nil {
				firstThreadErr = thread.err
			}
		}
	}
	if len(roots) > 0 && failedThreads == len(roots) {
		return nil, firstThreadErr
	}

	candidates := make([]fetchedHistoryThread, 0, len(threads)-failedThreads)
	for _, thread := range threads {
		if thread.err == nil && historyInRange(thread.latest, q.StartTime, q.EndTime) {
			candidates = append(candidates, thread)
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		if candidates[a].latest == candidates[b].latest {
			return candidates[a].root.ID < candidates[b].root.ID
		}
		return candidates[a].latest > candidates[b].latest
	})
	totalThreads := len(roots)
	threadCount := min(maxHistoryExpandedThreads, len(candidates))
	replyLimit := maxHistoryThreadReplies

	if err := g.backfillHistoryTexts(ctx, target, base); err != nil {
		return nil, err
	}
	for i := range candidates {
		messages := append([]core.Message{candidates[i].root}, candidates[i].replies...)
		if err := g.backfillHistoryTexts(ctx, target, messages); err != nil {
			return nil, err
		}
		candidates[i].root = messages[0]
		copy(candidates[i].replies, messages[1:])
	}

	for i := range candidates {
		candidates[i].root.Background = target.currentThread == "" || candidates[i].root.ThreadID != target.currentThread
	}
	budget := min(g.config.Limits.TextChars*2, 40000)
	var messages []core.Message
	var includedReplies, includedThreads int
	for {
		messages, includedReplies, includedThreads = buildExpandedHistory(base, candidates, threadCount, replyLimit)
		if historyTextLength(messages) <= budget {
			break
		}
		if replyLimit > 0 {
			replyLimit--
			continue
		}
		if threadCount > 0 {
			threadCount--
			continue
		}
		break
	}
	trimHistoryBudget(messages, budget)
	sort.SliceStable(messages, func(a, b int) bool {
		if messages[a].Time == messages[b].Time {
			return messages[a].ID > messages[b].ID
		}
		return messages[a].Time > messages[b].Time
	})
	if err := g.store.RememberContext(target.source.Key, messages); err != nil {
		return nil, err
	}
	availableReplies := 0
	for _, thread := range candidates {
		availableReplies += thread.available
	}
	omittedReplies := availableReplies - includedReplies
	if omittedReplies < 0 {
		omittedReplies = 0
	}
	omittedThreads := totalThreads - includedThreads
	threadIDs := make([]string, 0, len(roots))
	for _, root := range roots {
		threadIDs = append(threadIDs, root.ThreadID)
	}
	sort.Strings(threadIDs)
	g.auditChatHistory(target, threadIDs, len(messages))
	stats := core.Args{
		"threads":        totalThreads,
		"expanded":       includedThreads,
		"omittedThreads": omittedThreads,
		"omittedReplies": omittedReplies,
		"failedThreads":  failedThreads,
	}
	note := fmt.Sprintf("Expanded %d of %d discovered topics; %d threads failed and were skipped; %d replies were omitted. Only these topics were read; state this scope when summarizing.", includedThreads, totalThreads, failedThreads, omittedReplies)
	return g.historyChatResult(target, q, messages, token, stats, note), nil
}

func buildExpandedHistory(base []core.Message, threads []fetchedHistoryThread, threadCount, replyLimit int) ([]core.Message, int, int) {
	messages := append([]core.Message{}, base...)
	includedReplies := 0
	includedThreads := 0
	for i := 0; i < threadCount && i < len(threads); i++ {
		thread := threads[i]
		messages = append(messages, thread.root)
		includedThreads++
		n := min(replyLimit, len(thread.replies))
		for j := n - 1; j >= 0; j-- {
			messages = append(messages, thread.replies[j])
			includedReplies++
		}
	}
	return messages, includedReplies, includedThreads
}

func historyTextLength(messages []core.Message) int {
	total := 0
	for _, m := range messages {
		total += len([]rune(m.Text))
	}
	return total
}

func trimHistoryBudget(messages []core.Message, budget int) {
	remaining := budget
	for i := range messages {
		messages[i].Text = cut(messages[i].Text, min(remaining, 40000))
		remaining -= len([]rune(messages[i].Text))
		if remaining < 0 {
			remaining = 0
		}
		if len(messages[i].Resources) > 12 {
			messages[i].Resources = messages[i].Resources[:12]
		}
	}
}

func historyInRange(milliseconds, start, end int64) bool {
	if milliseconds <= 0 {
		return start == 0
	}
	seconds := milliseconds / 1000
	return (start == 0 || seconds >= start) && (end == 0 || seconds <= end)
}

func historyThreadIDs(messages []core.Message, extra string) []string {
	seen := map[string]bool{}
	out := []string{}
	if extra != "" {
		seen[extra] = true
		out = append(out, extra)
	}
	for _, m := range messages {
		if m.ThreadID != "" && !seen[m.ThreadID] {
			seen[m.ThreadID] = true
			out = append(out, m.ThreadID)
		}
	}
	sort.Strings(out)
	return out
}

func (g *Gateway) backfillHistoryTexts(ctx context.Context, target historyTarget, messages []core.Message) error {
	ids := []string{}
	seen := map[string]bool{}
	for _, m := range messages {
		if m.SenderIsBot && m.SenderIsSelf && historyCardTextMissing(m.Text) && m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	texts, err := g.store.HistoryReplyTextsChat(ctx, g.config.TenantID, g.config.AppID, target.chatID, ids)
	if err != nil {
		g.audit(target.source.Key, "history_backfill_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		return nil
	}
	for i := range messages {
		if text, ok := texts[messages[i].ID]; ok && messages[i].SenderIsBot && messages[i].SenderIsSelf && historyCardTextMissing(messages[i].Text) {
			messages[i].Text = text
		}
	}
	return nil
}

func (g *Gateway) auditChatHistory(target historyTarget, threads []string, count int) {
	if threads == nil {
		threads = []string{}
	}
	g.audit(target.source.Key, "history_chat_scope_read", core.Args{
		"room":    target.source.Key,
		"session": target.session,
		"threads": threads,
		"count":   count,
	})
}

func (g *Gateway) historyChatResult(target historyTarget, q core.HistoryQuery, messages []core.Message, next string, stats core.Args, note string) core.Args {
	result := core.Args{"messages": messages, "nextCursor": nil, "hasMore": next != "", "note": note}
	if next != "" {
		q.PageToken = next
		result["nextCursor"] = g.sealHistoryCursor(target.source.Key, target.session, q)
	}
	for key, value := range stats {
		result[key] = value
	}
	return result
}
