// Package gateway connects authorized chat inputs to reusable native Claude Code
// sessions. SQLite, rather than a second task scheduler, owns delivery state.
package gateway

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

type slot struct {
	mu                                  sync.Mutex
	events                              sync.Mutex
	room                                core.Room
	session                             core.Session
	workerReady                         chan struct{}
	worker                              core.Worker
	ctx                                 context.Context
	cancel                              context.CancelFunc
	live, busy, pinned, stopUnconfirmed bool
	background                          int
	lastActivity                        int64
	workspace                           core.Workspace
	deliveries                          map[string]core.Delivery
	order                               []string
	inputGroups                         map[string][]string
	accepted                            map[string]bool
	anchor                              string
	renders                             map[string]*render
	replyAnchors                        map[string]core.Delivery
	replyOrder                          []string
	compacting                          string
	requests                            map[string]bool
	activeRequests                      int
	titleSync                           *sessionTitleSync
}
type actionExecution struct {
	room        string
	done        chan struct{}
	unconfirmed bool
}
type dispatchWake struct{ running, dirty bool }
type imageWake struct {
	deadline int64
	timer    *time.Timer
}

// Gateway is safe for concurrent Feishu events, native output and shutdown.
// Constructing it performs no network or filesystem operations.
type Gateway struct {
	config                                                       core.Config
	store                                                        *store.Store
	transport                                                    core.Transport
	factory                                                      core.WorkerFactory
	logger                                                       core.Logger
	ctx                                                          context.Context
	cancel                                                       context.CancelFunc
	mu                                                           sync.Mutex
	actions                                                      map[string]*actionExecution
	actionRunner                                                 func(context.Context, string, []string, string, map[string]string, int64) (int, string, error)
	closing                                                      bool
	wg                                                           sync.WaitGroup
	slots                                                        map[string]*slot
	rates                                                        map[string][]int64
	waiting                                                      map[string]bool
	profileWait                                                  map[string]string
	wakes                                                        map[string]*dispatchWake
	imageTimers                                                  map[string]imageWake
	inputLocks, dispatchLocks, receiptLocks, approvalUpdateLocks keyedLocks
	outTargetLocks                                               keyedLocks
	statusLocks                                                  keyedLocks
	poolMu, sweepMu                                              sync.Mutex
	outMu                                                        sync.Mutex
	outActive                                                    map[string]bool
	outCursor                                                    map[string]outputCursor
	stopBlocked                                                  map[string]bool
	identityLost                                                 map[string]bool
	receiptMu                                                    sync.Mutex
	receiptActive                                                map[string]*dispatchWake
	receiptCursor                                                outputCursor
	approvalMu                                                   sync.Mutex
	approvals                                                    map[string]*approval
	historyKey                                                   [32]byte
	secrets                                                      []string
}

// New constructs the service. Recover must run before accepting new inputs.
func New(c core.Config, st *store.Store, tr core.Transport, factory core.WorkerFactory, log core.Logger) *Gateway {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Gateway{config: c, store: st, transport: tr, factory: factory, logger: log, ctx: ctx, cancel: cancel,
		actions: map[string]*actionExecution{}, actionRunner: runActionCommand, slots: map[string]*slot{}, rates: map[string][]int64{}, waiting: map[string]bool{}, profileWait: map[string]string{}, wakes: map[string]*dispatchWake{}, imageTimers: map[string]imageWake{}, outActive: map[string]bool{}, outCursor: map[string]outputCursor{}, stopBlocked: map[string]bool{}, identityLost: map[string]bool{}, receiptActive: map[string]*dispatchWake{}, approvals: map[string]*approval{}}
	if _, err := rand.Read(g.historyKey[:]); err != nil {
		panic("cannot initialize history cursor authentication")
	}
	g.secrets = config.Secrets(c)
	g.task(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				g.Sweep(ctx, now.UnixMilli())
			}
		}
	})
	return g
}
func (g *Gateway) task(fn func()) bool {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return false
	}
	g.wg.Add(1)
	g.mu.Unlock()
	go func() { defer g.wg.Done(); fn() }()
	return true
}
func (g *Gateway) isClosing() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.closing }
func (g *Gateway) audit(room, event string, a core.Args) {
	if a == nil {
		a = core.Args{}
	}
	a = g.redactArgs(a)
	if e := g.store.Audit(room, event, a); e != nil && g.logger != nil {
		g.logger("audit_failed", core.Args{"event": event, "error": core.Redact(e.Error(), g.secrets)})
	}
	if g.logger != nil {
		g.logger(event, a)
	}
}
func (g *Gateway) getSlot(room string) *slot { g.mu.Lock(); defer g.mu.Unlock(); return g.slots[room] }
func (g *Gateway) removeSlot(s *slot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.slots[s.room.Key] == s {
		for _, a := range g.actions {
			if a.room == s.room.Key {
				return
			}
		}
		delete(g.slots, s.room.Key)
	}
}
func (g *Gateway) allSlots() []*slot {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := make([]*slot, 0, len(g.slots))
	for _, s := range g.slots {
		v = append(v, s)
	}
	return v
}
func (g *Gateway) active(s *slot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live && s.ctx.Err() == nil
}

func (g *Gateway) Recover(ctx context.Context) error {
	if err := g.recoverInterrupted(ctx); err != nil {
		return err
	}
	if recovery, ok := g.transport.(core.CardRecovery); ok {
		repairCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		count, err := recovery.RecoverCards(repairCtx)
		cancel()
		if err != nil {
			g.audit("", "interrupted_card_repair_pending", core.Args{"recovered": count})
		}
	}
	pending, err := g.store.PendingRooms()
	if err != nil {
		return err
	}
	for _, key := range pending {
		r, ok := RoomFromKey(g.config, key)
		ds, e := g.store.Pending(key)
		if e != nil {
			return e
		}
		for _, d := range ds {
			if !ok || !g.sanitize(r, &d) {
				_ = g.store.DiscardInput(key, d.Message.ID)
				g.settleReceipts(key, d.SessionID, origin(d.Message), false)
			} else if d.SupplementTo != "" {
				g.cancelSupplement(r, d, "服务已重启，原前台运行不再存在。", false, "queued")
			} else if e = g.store.Queue(d); e != nil {
				return e
			}
		}
	}
	if _, err = g.store.DB.Exec("UPDATE receipts SET desired='clear' WHERE desired='processing'"); err != nil {
		return err
	}
	g.retryReceipts()
	// Existing durable outputs also need discovery when there is no queued input.
	rows, err := g.store.DB.Query("SELECT DISTINCT room FROM outbox WHERE state IN ('pending','uncertain')")
	if err != nil {
		return err
	}
	var outputRooms []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			outputRooms = append(outputRooms, k)
		}
	}
	rows.Close()
	for _, key := range outputRooms {
		if r, ok := RoomFromKey(g.config, key); ok {
			for _, id := range g.retryable(key) {
				id := id
				g.task(func() { _, _ = g.deliverOutput(g.ctx, id, r, false) })
			}
		}
	}
	g.kickWaiting()
	return ctx.Err()
}
func (g *Gateway) sanitize(r core.Room, d *core.Delivery) bool {
	current, ok := routeQueued(g.config, r.Key, d.Message)
	if !ok || !Triggers(current, d.Message) || d.Message.InputIssue != "" {
		return false
	}
	if d.SupplementTo != "" && d.Command != nil {
		return false
	}
	if d.Command != nil {
		cmd, _ := splitCommand(d.Message.Text)
		if d.Command.Name != "compact" || cmd != "/compact" || !canManage(current, d.Message.SenderID) {
			return false
		}
	}
	bg := []core.Message{}
	if g.config.Limits.BackgroundMessages > 0 {
		for _, m := range d.Background {
			if Readable(current, m) {
				bg = append(bg, m)
			}
		}
		if len(bg) > g.config.Limits.BackgroundMessages {
			bg = bg[len(bg)-g.config.Limits.BackgroundMessages:]
		}
	}
	d.Background = bg
	if d.Quote != nil && !Readable(current, *d.Quote) {
		d.Quote = nil
	}
	d.Attachments = nil
	return true
}
func (g *Gateway) Message(ctx context.Context, original core.Message) error {
	if !g.beginCall() {
		return nil
	}
	defer g.wg.Done()
	room, ok := Route(g.config, original)
	if !ok || (!Triggers(room, original) && g.config.Limits.BackgroundMessages == 0) {
		return nil
	}
	if room.Thread != "" && original.ThreadID == "" && !Triggers(room, original) {
		// In a thread-scoped group only a trigger starts a topic; top-level
		// chatter never creates a room or background context.
		return nil
	}
	unlock := g.inputLocks.lock(room.Key)
	defer unlock()
	if g.isClosing() {
		return nil
	}
	seen, err := g.store.Seen(room.Key, original.ID)
	if err != nil || seen {
		return err
	}
	m := original
	m.Text = cut(strings.TrimSpace(m.Text), g.config.Limits.TextChars)
	if len(m.Resources) > 12 {
		m.Resources = m.Resources[:12]
	}
	if !Triggers(room, m) {
		if g.config.Limits.BackgroundMessages > 0 && m.InputIssue == "" {
			if _, err = g.store.Insert(room.Key, m, "background"); err != nil {
				return err
			}
			_, err = g.store.DB.Exec("DELETE FROM inbox WHERE room=? AND status='background' AND id NOT IN (SELECT id FROM inbox WHERE room=? AND status='background' ORDER BY at DESC,id DESC LIMIT ?)", room.Key, room.Key, g.config.Limits.BackgroundMessages)
		}
		return err
	}
	if !imageEligible(m) {
		if err = g.sealImages(room.Key); err != nil {
			return err
		}
	}
	cmd, arg := splitCommand(m.Text)
	now := time.Now().UnixMilli()
	if cmd != "/stop" || !canManage(room, m.SenderID) {
		g.mu.Lock()
		// Topics split sessions, not the author's message budget: rate per
		// chat and author, whatever topic each message opens or joins.
		key := room.WorkspaceKey() + ":" + m.SenderID
		recent := g.rates[key][:0]
		for _, at := range g.rates[key] {
			if at > now-60000 {
				recent = append(recent, at)
			}
		}
		limited := len(recent) >= g.config.Limits.MessagesPerMinute
		if !limited {
			recent = append(recent, now)
		}
		g.rates[key] = recent
		g.mu.Unlock()
		if limited {
			_, err = g.store.Insert(room.Key, m, "cancelled")
			g.notify(g.refusalRoom(room, m), "消息过于频繁，请稍后再试。", &m)
			return err
		}
	}
	if m.InputIssue != "" || len([]rune(strings.TrimSpace(original.Text))) > g.config.Limits.TextChars || len(original.Resources) > 12 {
		m.Text = ""
		m.Resources = nil
		_, err = g.store.Insert(room.Key, m, "cancelled")
		reason := m.InputIssue
		if reason == "" {
			reason = "正文或附件数量超过本次输入上限，请分开发送。"
		}
		g.notify(g.refusalRoom(room, m), "消息未提交："+reason, &m)
		return err
	}
	if room.Thread != "" && m.ThreadID == "" && contains(topicCommands, cmd) {
		// A top-level command in a thread-scoped group has no topic to act on.
		// Never guess the most recent topic; answer on the chat timeline.
		if _, err = g.store.Insert(room.Key, m, "done"); err != nil {
			return err
		}
		if chat, ok := RoomFor(g.config, room.ChatID, ""); ok {
			g.notify(chat, "本群按话题会话运行："+cmd+" 只作用于单个话题，请在对应话题内 @ 我发送。在群里直接 @ 我提出新问题会开启新话题和新会话。", &m)
		}
		return nil
	}
	if contains([]string{"/help", "/status", "/sessions", "/new", "/resume", "/stop", "/answer", "/outputs", "/retry-output", "/usage", "/model", "/profile", "/rename", "/artifacts", "/inputs", "/cancel-input", "/discard-output"}, cmd) {
		if _, err = g.store.Insert(room.Key, m, "done"); err != nil {
			return err
		}
		if contains([]string{"/help", "/status", "/sessions", "/outputs", "/usage", "/retry-output", "/artifacts", "/inputs", "/cancel-input", "/discard-output"}, cmd) {
			g.task(func() {
				if e := g.command(g.ctx, room, m); e != nil {
					g.audit(room.Key, "management_failed", core.Args{"error": core.Redact(e.Error(), g.secrets)})
				}
			})
			return nil
		}
		return g.command(ctx, room, m)
	}
	supplementTo := ""
	if cmd == "/btw" || cmd == "/ps" {
		if arg == "" {
			_, err = g.store.Insert(room.Key, m, "done")
			g.notify(room, "用法：/btw 补充正文（/ps 同义）。仅补充当前正在执行的前台任务。", &m)
			return err
		}
		target := g.foregroundTarget(room)
		if target == nil {
			_, err = g.store.Insert(room.Key, m, "cancelled")
			g.notify(room, "当前没有正在执行的前台任务，补充未提交。需要开始任务时请发送普通消息。", &m)
			return err
		}
		supplementTo = target.UUID
		m.Text = arg
	}
	count, err := g.store.CountPending(room.Key)
	if err != nil {
		return err
	}
	total, err := g.pendingInChat(room)
	if err != nil {
		return err
	}
	if total >= g.config.Limits.InboxPerRoom {
		_, err = g.store.Insert(room.Key, m, "cancelled")
		text := "本群待交付输入已达上限，请查看 /status 或联系维护者。"
		if room.Thread != "" {
			text = "本群所有话题的待交付输入合计已达上限，请在各话题内用 /status 查看，或等待已有任务完成后再发送。"
		}
		g.notify(g.refusalRoom(room, m), text, &m)
		return err
	}
	var command *core.Command
	if supplementTo == "" && cmd == "/compact" {
		if !canManage(room, m.SenderID) {
			_, err = g.store.Insert(room.Key, m, "cancelled")
			g.notify(room, "整理共享会话需要维护者操作。", &m)
			return err
		}
		command = &core.Command{Name: "compact", Instructions: cut(arg, 4000)}
	}
	session, err := g.store.Session(room.Key)
	if err != nil {
		return err
	}
	bg := []core.Message{}
	if command == nil && supplementTo == "" {
		bg, err = g.store.Background(room.Key, now, g.config.Limits.BackgroundMS, g.config.Limits.BackgroundMessages)
		if err != nil {
			return err
		}
		filtered := bg[:0]
		for _, m := range bg {
			if Readable(room, m) {
				filtered = append(filtered, m)
			}
		}
		bg = filtered
		total := 0
		for i := len(bg) - 1; i >= 0; i-- {
			total += len([]rune(bg[i].Text))
			if total > min(g.config.Limits.TextChars*2, 40000) {
				bg = bg[i+1:]
				break
			}
		}
	}
	d := core.Delivery{UUID: core.NewID(), RoomKey: room.Key, SessionID: session.ID, Message: m, Background: bg, Command: command, SupplementTo: supplementTo}
	err = g.queueInput(d, now)
	if err != nil {
		return err
	}
	g.acceptReceipt(room, session.ID, m)
	if g.stopIsBlocked(room.Key) {
		g.notify(room, stopBlockedNotice, &m)
		return nil
	}
	g.kick(room)
	if command != nil && count > 0 {
		g.notify(room, "已接收 /compact，将在当前工作轮次结束后整理上下文；期间仍可处理审批。", &m)
	}
	return nil
}

// pendingInChat counts pending inputs for the chat that owns r. Topic rooms
// of one chat share the chat's inboxPerRoom budget, as a chat room did before
// topics existed.
func (g *Gateway) pendingInChat(r core.Room) (int, error) {
	if r.Thread == "" {
		return g.store.CountPending(r.Key)
	}
	return g.store.CountPendingChat(r.ChatKey)
}

// stopStateNotice explains a stop whose process part completed while its
// state update failed; stopBlockedNotice answers inputs that arrive meanwhile.
const (
	stopStateNotice   = "原生进程已停止，但取消待交付内容的状态未能保存，因此本聊天暂停交付、未切换会话。请检查服务诊断后重新发送 /stop；暂停只在本次服务运行内有效。"
	stopBlockedNotice = "本聊天上次停止未能完成（状态保存失败），暂不交付新输入。维护者重新发送 /stop 成功后，暂存的输入会随停止一并取消，届时请重新发送；若期间服务重启，暂存输入会被正常交付。"
)

// refusalRoom picks where to explain a refused input. A refused top-level
// message in a thread-scoped group never became a topic, so the notice replies
// on the chat timeline instead of opening a topic (which Feishu also refuses
// for aggregated messages such as merge forwards).
func (g *Gateway) refusalRoom(room core.Room, m core.Message) core.Room {
	if room.Thread == "" || m.ThreadID != "" {
		return room
	}
	if chat, ok := RoomFor(g.config, room.ChatID, ""); ok {
		return chat
	}
	return room
}

// topicCommands act on one session. In a thread-scoped group they are
// accepted only inside the topic that owns that session.
var topicCommands = []string{"/status", "/sessions", "/new", "/resume", "/stop", "/answer", "/outputs", "/retry-output", "/usage", "/model", "/profile", "/rename", "/artifacts", "/inputs", "/cancel-input", "/discard-output", "/compact", "/btw", "/ps"}

func (g *Gateway) foregroundTarget(r core.Room) *core.Delivery {
	s := g.getSlot(r.Key)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live || !s.busy || s.compacting != "" || s.anchor == "" {
		return nil
	}
	d, ok := s.deliveries[s.anchor]
	if !ok || d.Command != nil {
		return nil
	}
	return &d
}
func (g *Gateway) supplementAlive(r core.Room, d core.Delivery) bool {
	if d.SupplementTo == "" {
		return true
	}
	s := g.getSlot(r.Key)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.deliveries[d.SupplementTo]
	return s.live && s.busy && s.session.ID == d.SessionID && ok
}
func (g *Gateway) cancelSupplement(r core.Room, d core.Delivery, reason string, deliver bool, status string) bool {
	text := "补充已取消：" + reason + " 未提交给模型，也不会转成新任务。"
	id := ""
	changed := false
	e := g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		res, e := tx.Exec("UPDATE inbox SET status='cancelled',payload=NULL WHERE uuid=? AND room=? AND status=?", d.UUID, r.Key, status)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil
		}
		changed = true
		current, ok := routeQueued(g.config, r.Key, d.Message)
		if ok && Triggers(current, d.Message) {
			id, e = g.saveOutput(tx, r, d.SessionID, text, false, "", messageReply(d.Message), d.Message.ThreadID, DisplayCard("补充已取消", text), origin(d.Message), "", "", "recovery", "")
		} else {
			_, e = tx.Exec("UPDATE inbox SET message='{}' WHERE uuid=?", d.UUID)
		}
		return e
	})
	if e != nil || !changed {
		return false
	}
	g.settleReceipts(r.Key, d.SessionID, origin(d.Message), false)
	if id != "" && deliver {
		g.task(func() { _, _ = g.deliverOutput(g.ctx, id, r, false) })
	}
	return true
}
func (g *Gateway) cancelLostSupplements(r core.Room) {
	ds, e := g.store.Pending(r.Key)
	if e != nil {
		return
	}
	for _, d := range ds {
		if d.SupplementTo != "" && !g.supplementAlive(r, d) {
			g.cancelSupplement(r, d, "目标前台运行已经结束或停止。", true, "queued")
		}
	}
}
func (g *Gateway) kick(r core.Room) {
	g.mu.Lock()
	if g.closing || g.profileWait[r.Key] != "" || g.stopBlocked[r.Key] {
		g.mu.Unlock()
		return
	}
	w := g.wakes[r.Key]
	if w != nil {
		w.dirty = true
		g.mu.Unlock()
		return
	}
	w = &dispatchWake{running: true}
	g.wakes[r.Key] = w
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.wg.Done()
		for {
			unlock := g.dispatchLocks.lock(r.Key)
			err := g.dispatch(r)
			unlock()
			if err != nil && !g.isClosing() {
				g.audit(r.Key, "dispatch_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
				if errors.Is(err, config.ErrModelProfileUnavailable) {
					// The saved session or requested profile no longer exists.
					// Keep the input without a silent model switch, but stop the
					// 30-second sweep from re-dispatching and re-notifying. /model,
					// /new or /resume clears the block after a deliberate change.
					g.mu.Lock()
					first := g.profileWait[r.Key] == ""
					g.profileWait[r.Key] = err.Error()
					delete(g.wakes, r.Key)
					g.mu.Unlock()
					if first {
						g.notify(r, "已保存的模型配置不可用；输入已保存，不会自动换模型。请用 /model 选择可用配置或 default，或恢复管理员配置。", nil)
					}
					return
				}
				g.notify(r, "输入已保存，但运行启动或交付失败；请查看服务诊断。", nil)
			}
			g.mu.Lock()
			if !w.dirty || g.closing {
				delete(g.wakes, r.Key)
				g.mu.Unlock()
				return
			}
			w.dirty = false
			g.mu.Unlock()
		}
	}()
}
func (g *Gateway) clearProfileWait(key string) {
	g.mu.Lock()
	delete(g.profileWait, key)
	g.mu.Unlock()
}

func (g *Gateway) kickWaiting() {
	if g.isClosing() {
		return
	}
	keys, e := g.store.PendingRooms()
	if e != nil {
		return
	}
	for _, key := range keys {
		r, ok := RoomFromKey(g.config, key)
		if ok {
			g.kick(r)
		}
	}
}
func (g *Gateway) clearImageTimer(key string) {
	g.mu.Lock()
	if w, ok := g.imageTimers[key]; ok {
		w.timer.Stop()
		delete(g.imageTimers, key)
	}
	g.mu.Unlock()
}
func (g *Gateway) waitImages(r core.Room, d core.Delivery) bool {
	b := d.ImageBatch
	now := time.Now().UnixMilli()
	if b == nil || b.Closed || g.config.Limits.ImageBatchMS == 0 || b.Deadline <= now {
		g.clearImageTimer(r.Key)
		return false
	}
	g.mu.Lock()
	if old, ok := g.imageTimers[r.Key]; !ok || old.deadline != b.Deadline {
		if ok {
			old.timer.Stop()
		}
		deadline := b.Deadline
		t := time.AfterFunc(time.Duration(max(int64(1), min(int64(5000), deadline-now)))*time.Millisecond, func() { g.mu.Lock(); delete(g.imageTimers, r.Key); g.mu.Unlock(); g.kick(r) })
		g.imageTimers[r.Key] = imageWake{deadline, t}
	}
	g.mu.Unlock()
	return true
}

func (g *Gateway) reserve(r core.Room) (*slot, error) {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	if g.isClosing() {
		return nil, nil
	}
	if s := g.getSlot(r.Key); s != nil {
		return s, nil
	}
	slots := g.allSlots()
	if len(slots) >= g.config.Runtime.MaxWorkers {
		sort.Slice(slots, func(i, j int) bool {
			slots[i].mu.Lock()
			a := slots[i].lastActivity
			slots[i].mu.Unlock()
			slots[j].mu.Lock()
			b := slots[j].lastActivity
			slots[j].mu.Unlock()
			return a < b
		})
		found := false
		for _, s := range slots {
			if g.reclaimable(s) {
				found = g.releaseIdle(g.ctx, s)
				break
			}
		}
		if !found {
			return nil, nil
		}
	}
	ss, e := g.store.Session(r.Key)
	if e != nil {
		return nil, e
	}
	if e = g.sessionProfile(&ss); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(g.ctx)
	s := &slot{room: r, session: ss, ctx: ctx, cancel: cancel, live: true, workerReady: make(chan struct{}), background: -1, lastActivity: time.Now().UnixMilli(), deliveries: map[string]core.Delivery{}, inputGroups: map[string][]string{}, accepted: map[string]bool{}, renders: map[string]*render{}, replyAnchors: map[string]core.Delivery{}, requests: map[string]bool{}}
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		cancel()
		return nil, nil
	}
	g.slots[r.Key] = s
	delete(g.waiting, r.Key)
	g.mu.Unlock()
	return s, nil
}
func (g *Gateway) dispatch(r core.Room) error {
	if g.isClosing() || g.stopIsBlocked(r.Key) {
		// A kick may have been waiting on the dispatch lock while a stop
		// failed; the block must hold for it too.
		return nil
	}
	_, profileErr := g.prepareProfile(r)
	if profileErr != nil {
		return profileErr
	}
	g.cancelLostSupplements(r)
	pending, e := g.store.Pending(r.Key)
	if e != nil {
		return e
	}
	valid := make([]core.Delivery, 0, len(pending))
	allowed := make([]bool, len(pending))
	invalidBatches := map[string]bool{}
	for i := range pending {
		allowed[i] = g.sanitize(r, &pending[i])
		if !allowed[i] && pending[i].ImageBatch != nil {
			invalidBatches[pending[i].ImageBatch.ID] = true
		}
	}
	for i, d := range pending {
		if !allowed[i] || (d.ImageBatch != nil && invalidBatches[d.ImageBatch.ID]) {
			_ = g.store.DiscardInput(r.Key, d.Message.ID)
			g.settleReceipts(r.Key, d.SessionID, origin(d.Message), false)
		} else {
			valid = append(valid, d)
		}
	}
	pending, e = g.profileInputs(r, valid)
	if e != nil {
		return e
	}
	if len(pending) == 0 {
		g.clearImageTimer(r.Key)
		return nil
	}
	if g.waitImages(r, pending[0]) {
		return nil
	}
	s, e := g.reserve(r)
	if e != nil {
		return e
	}
	if s == nil {
		g.mu.Lock()
		already := g.waiting[r.Key]
		g.waiting[r.Key] = true
		g.mu.Unlock()
		if !already {
			g.notify(r, "当前会话都在工作、监控或停止尚未确认，输入已保存。正常空闲后会自动处理；停止未确认时请维护者检查服务诊断。", &pending[0].Message)
		}
		return nil
	}
	s.mu.Lock()
	worker := s.worker
	unconfirmed := s.stopUnconfirmed
	s.mu.Unlock()
	if unconfirmed {
		return nil
	}
	if worker == nil {
		p, _ := g.config.Project(r.ProjectID)
		w, err := workspace.PrepareContext(s.ctx, g.config, r, s.session, p)
		if err == nil {
			err = g.store.BindLaunchIdentity(s.session, w, p.CWD != "")
		}
		if err != nil {
			s.mu.Lock()
			s.live = false
			if errors.Is(err, process.ErrUnconfirmed) {
				s.stopUnconfirmed = true
			}
			keep := s.stopUnconfirmed
			s.mu.Unlock()
			s.cancel()
			if !keep {
				g.removeSlot(s)
			}
			return err
		}
		if s.session.CCID == "" && !g.sessionIdentityLost(s.session.ID) {
			// An earlier process may have reported its native session while
			// the database rejected the ID; adopt the workspace marker so this
			// launch resumes that context instead of starting another.
			ccid, ok, markerErr := readIdentityMarker(w.Root, s.session.ID)
			if markerErr != nil {
				// An unreadable marker is not an absent one: refuse below.
				g.audit(r.Key, "session_identity_marker_unreadable", core.Args{"session": s.session.ID, "error": markerErr.Error()})
				g.mu.Lock()
				g.identityLost[s.session.ID] = true
				g.mu.Unlock()
			} else if ok {
				if err = g.store.SetCC(s.session.ID, ccid); err != nil {
					s.mu.Lock()
					s.live = false
					s.mu.Unlock()
					s.cancel()
					g.removeSlot(s)
					return err
				}
				s.mu.Lock()
				s.session.CCID = ccid
				s.mu.Unlock()
				removeIdentityMarker(w.Root, s.session.ID)
				g.audit(r.Key, "session_identity_recovered", core.Args{"session": s.session.ID})
			}
		}
		if s.session.CCID == "" && g.sessionIdentityLost(s.session.ID) {
			s.mu.Lock()
			s.live = false
			s.mu.Unlock()
			s.cancel()
			g.removeSlot(s)
			// These inputs never reached a process; cancel them so later
			// sweeps do not repeat the refusal.
			if err := g.store.StopSession(s.session.ID); err != nil {
				return err
			}
			// Inputs queued during preparation were cancelled too; clear
			// every processing reaction of this session, not only pending's.
			g.cancelReceipts(s.session.ID)
			g.notify(r, identityFailureNotice, &pending[0].Message)
			return nil
		}
		s.mu.Lock()
		s.workspace = w
		s.mu.Unlock()
		worker, err = g.factory(s.ctx, r, s.session, w, func(ev core.WorkerEvent) { g.event(s, ev) })
		if err != nil {
			s.mu.Lock()
			s.live = false
			if errors.Is(err, process.ErrUnconfirmed) {
				s.stopUnconfirmed = true
			}
			keep := s.stopUnconfirmed
			s.mu.Unlock()
			s.cancel()
			if !keep {
				g.removeSlot(s)
			}
			return err
		}
		s.mu.Lock()
		s.worker = worker
		close(s.workerReady)
		live := s.live
		s.mu.Unlock()
		if !live || g.isClosing() {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
			err = providerVoid(stopCtx, func() error { return worker.Stop(stopCtx) })
			cancelStop()
			if err != nil {
				s.mu.Lock()
				s.stopUnconfirmed = true
				s.mu.Unlock()
				return err
			}
			g.removeSlot(s)
			return nil
		}
		g.applySessionTitle(s, worker, pending[0].Message)
	}
	for g.active(s) && !g.isClosing() {
		queued, err := g.store.Pending(r.Key)
		if err != nil {
			return err
		}
		queued, err = g.profileInputs(r, queued)
		if err != nil {
			return err
		}
		if len(queued) == 0 {
			g.clearImageTimer(r.Key)
			return nil
		}
		d := queued[0]
		if g.waitImages(r, d) {
			return nil
		}
		members, err := imageMembers(queued, d)
		if err != nil {
			bad := []core.Delivery{d}
			if d.ImageBatch != nil {
				bad = nil
				for _, member := range queued {
					if member.ImageBatch != nil && member.ImageBatch.ID == d.ImageBatch.ID {
						bad = append(bad, member)
					}
				}
			}
			g.rejectBatch(r, bad, true)
			continue
		}
		if d.SessionID != s.session.ID {
			for _, m := range members {
				_ = g.store.Mark(m.UUID, "cancelled")
				g.settleReceipts(r.Key, m.SessionID, origin(m.Message), false)
			}
			continue
		}
		authorized := true
		for i := range members {
			if !g.sanitize(r, &members[i]) {
				authorized = false
			}
		}
		if !authorized {
			g.rejectBatch(r, members, false)
			continue
		}
		if !g.supplementAlive(r, d) {
			g.cancelSupplement(r, d, "目标前台运行已经结束或停止。", true, "queued")
			continue
		}
		s.mu.Lock()
		blocked := s.compacting != "" || (d.Command != nil && (s.busy || len(s.deliveries) > 0 || s.activeRequests > 0))
		s.mu.Unlock()
		if blocked || (d.Command != nil && g.approvalCount(s.session.ID) > 0) {
			return nil
		}
		if d.ImageBatch != nil {
			for i := range members {
				members[i].ImageBatch.Closed = true
				if e = g.store.Queue(members[i]); e != nil {
					return e
				}
			}
		}
		if g.stopIsBlocked(r.Key) {
			// A failed stop may have landed after this pass began.
			return nil
		}
		handedOff := false
		paths := []string{}
		err = g.prepareInputs(s, r, members, &paths)
		if err == nil {
			err = g.store.Transaction(s.ctx, func(tx *sql.Tx) error {
				for _, m := range members {
					res, e := tx.Exec("UPDATE inbox SET status='sent' WHERE uuid=? AND room=? AND session=? AND status='queued'", m.UUID, r.Key, s.session.ID)
					if e != nil {
						return e
					}
					n, _ := res.RowsAffected()
					if n != 1 {
						return errors.New("input preparation cancelled")
					}
				}
				return nil
			})
		}
		if err == nil && g.active(s) {
			s.mu.Lock()
			for _, m := range members {
				s.deliveries[m.UUID] = m
				s.order = append(s.order, m.UUID)
				a := m
				a.Message.Text = ""
				a.Message.Resources = nil
				a.Background = nil
				a.Quote = nil
				a.Attachments = nil
				a.BatchInputs = nil
				s.replyAnchors[m.UUID] = a
				s.replyOrder = append(s.replyOrder, m.UUID)
			}
			if d.ImageBatch != nil {
				ids := []string{}
				for _, m := range members {
					ids = append(ids, m.UUID)
				}
				s.inputGroups[d.UUID] = ids
			}
			for len(s.replyOrder) > 10000 {
				delete(s.replyAnchors, s.replyOrder[0])
				s.replyOrder = s.replyOrder[1:]
			}
			if s.anchor == "" {
				s.anchor = d.UUID
			}
			if d.Command != nil {
				s.compacting = d.UUID
			}
			s.busy = true
			s.lastActivity = time.Now().UnixMilli()
			s.mu.Unlock()
			for _, m := range members {
				g.acceptReceipt(r, s.session.ID, m.Message)
			}
			wire := members[0]
			if d.ImageBatch != nil {
				wire.BatchInputs = members
			}
			handedOff = true
			err = worker.Send(s.ctx, wire)
		}
		if !handedOff {
			for _, path := range paths {
				_ = os.Remove(path)
			}
		}
		if err != nil {
			if !g.active(s) || g.isClosing() {
				return nil
			}
			if !handedOff && d.SupplementTo != "" && !g.supplementAlive(r, d) {
				g.cancelSupplement(r, d, "目标前台运行已在交付前结束。", true, "queued")
				continue
			}
			if !handedOff && d.ImageBatch != nil {
				g.rejectBatch(r, members, true)
				continue
			}
			if !handedOff {
				// Preparation failed before the queued→sent commit, so the
				// model definitely never received these inputs. Report that
				// plainly instead of as an unconfirmed delivery.
				stateErr := error(nil)
				for _, m := range members {
					if _, e := g.store.DB.Exec("UPDATE inbox SET status='cancelled',payload=NULL WHERE uuid=? AND room=? AND status='queued'", m.UUID, r.Key); e != nil {
						if e = g.store.Mark(m.UUID, "uncertain"); e != nil {
							stateErr = e
						}
					}
					g.settleReceipts(r.Key, m.SessionID, origin(m.Message), false)
				}
				if stateErr != nil {
					// The input is still queued; picking it up again at once
					// would loop. Leave it for a later dispatch.
					g.audit(r.Key, "input_state_unconfirmed", core.Args{"session": s.session.ID})
					return stateErr
				}
				g.notify(r, "本条输入未提交给模型："+core.Redact(err.Error(), g.secrets)+"。请处理后重新发送。", &d.Message)
				continue
			}
			for _, m := range members {
				_ = g.store.Mark(m.UUID, "uncertain")
				g.settleReceipts(r.Key, m.SessionID, origin(m.Message), false)
			}
			g.notify(r, "本条交付未确认，不会自动重放："+core.Redact(err.Error(), g.secrets), &d.Message)
			if handedOff {
				stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
				stopErr := providerVoid(stopCtx, func() error { return worker.Stop(stopCtx) })
				cancelStop()
				confirmed := stopErr == nil
				g.event(s, core.WorkerEvent{Type: "exit", CleanupConfirmed: &confirmed})
				return nil
			}
		}
		if d.Command != nil {
			return nil
		}
	}
	return nil
}
func safeAttachmentName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	base = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, base)
	base = cut(base, 100)
	if base == "" || base == "." || base == ".." {
		return "attachment"
	}
	return base
}
func (g *Gateway) stillQueued(d core.Delivery) bool {
	var n int
	e := g.store.DB.QueryRow("SELECT 1 FROM inbox WHERE uuid=? AND room=? AND session=? AND status='queued'", d.UUID, d.RoomKey, d.SessionID).Scan(&n)
	return e == nil && n == 1
}
func (g *Gateway) prepareInputs(s *slot, r core.Room, members []core.Delivery, paths *[]string) error {
	batch := members[0].ImageBatch
	totalImages := 0
	for _, d := range members {
		totalImages += len(d.Message.Resources)
	}
	if batch != nil && totalImages > min(batch.MaxImages, g.config.Limits.ImageBatchMaxImages) {
		return errors.New("image count limit")
	}
	total := int64(0)
	check := func() error {
		if !g.active(s) {
			return context.Canceled
		}
		for _, d := range members {
			current, ok := routeQueued(g.config, r.Key, d.Message)
			if !ok || !Triggers(current, d.Message) || !g.stillQueued(d) || !g.supplementAlive(r, d) {
				return errors.New("input preparation cancelled")
			}
		}
		return nil
	}
	for i := range members {
		d := &members[i]
		if e := check(); e != nil {
			return e
		}
		quote := d.Message.ReplyTo
		if quote != "" && d.Command == nil && r.Thread != "" && d.Message.ThreadID != "" && quote == r.Thread {
			// Inside a topic Feishu sets parent_id to the topic root on every
			// reply. Supply the root until this session has actually received an
			// input, e.g. when a session joins a topic started without the bot or
			// after /new; a cached root alone does not mean the model saw it.
			received, e := g.store.SessionReceivedInput(r.Key, d.SessionID)
			if e != nil {
				return e
			}
			if received {
				quote = ""
			}
		}
		if quote != "" && d.Command == nil {
			quoted, e := provider(s.ctx, func() (*core.Message, error) { return g.transport.Fetch(s.ctx, quote, r) })
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			if e == nil && quoted != nil && Readable(r, *quoted) {
				quoted.Text = cut(quoted.Text, g.config.Limits.TextChars)
				if len(quoted.Resources) > 12 {
					quoted.Resources = quoted.Resources[:12]
				}
				d.Quote = quoted
				if e = g.store.RememberContext(r.Key, []core.Message{*quoted}); e != nil {
					return e
				}
			}
		}
		for index, res := range d.Message.Resources {
			if e := check(); e != nil {
				return e
			}
			name := safeAttachmentName(res.Name)
			path := filepath.Join(s.workspace.Inbound, fmt.Sprintf("%s-%d-%s", core.Hash(d.Message.ID)[:16], index, name))
			*paths = append(*paths, path)
			e := providerVoid(s.ctx, func() error {
				err := g.transport.Download(s.ctx, d.Message.ID, res, path, g.config.Limits.AttachmentBytes)
				// An adapter may complete after cancellation. Clean its late file
				// without touching SQLite or reviving the cancelled input.
				if s.ctx.Err() != nil {
					_ = os.Remove(path)
					return s.ctx.Err()
				}
				return err
			})
			if e != nil {
				return e
			}
			if e = check(); e != nil {
				return e
			}
			info, e := os.Stat(path)
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() || info.Size() > g.config.Limits.AttachmentBytes {
				return errors.New("附件超过允许大小")
			}
			total += info.Size()
			if batch != nil && total > min(batch.MaxBytes, g.config.Limits.ImageBatchBytes) {
				return errors.New("image batch byte limit")
			}
			d.Attachments = append(d.Attachments, core.Attachment{Path: path, Name: res.Name})
		}
	}
	return check()
}
func (g *Gateway) rejectBatch(r core.Room, members []core.Delivery, explain bool) {
	remaining := []core.Delivery{}
	for _, d := range members {
		if g.stillQueued(d) {
			remaining = append(remaining, d)
		}
	}
	if len(remaining) == 0 {
		return
	}
	id := ""
	e := g.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		for _, d := range remaining {
			q := "UPDATE inbox SET status='cancelled',payload=NULL WHERE uuid=?"
			if !explain {
				q = "UPDATE inbox SET status='cancelled',payload=NULL,message='{}' WHERE uuid=?"
			}
			if _, e := tx.Exec(q, d.UUID); e != nil {
				return e
			}
		}
		if explain {
			d := remaining[0]
			text := "图片批次未提交：附件读取失败或超过图片／大小限制。整批未交给模型，请检查文件和配置后重新发送。"
			var e error
			id, e = g.saveOutput(tx, r, d.SessionID, text, false, "", d.Message.ID, d.Message.ThreadID, DisplayCard("图片批次未提交", text), origins(remaining), "", "", "recovery", "")
			return e
		}
		return nil
	})
	if e != nil {
		g.audit(r.Key, "input_cancellation_failed", core.Args{"error": e.Error()})
		return
	}
	for _, d := range remaining {
		g.settleReceipts(r.Key, d.SessionID, origin(d.Message), false)
	}
	if id != "" {
		g.task(func() { _, _ = g.deliverOutput(g.ctx, id, r, false) })
	}
}
