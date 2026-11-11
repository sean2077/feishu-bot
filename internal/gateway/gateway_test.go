package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixtureWait bounds fixture-level event waits. It is a ceiling, not a delay:
// a normal run returns as soon as the event arrives. Race-enabled CI runners
// under load can take well over three seconds to start a worker or deliver the
// first input, so keep one generous budget instead of per-test values.
const fixtureWait = 20 * time.Second

type sentCard struct {
	id            string
	room          core.Room
	card          core.Args
	reply, thread string
}
type receiptCall struct{ id, desired, applied string }
type fakeTransport struct {
	mu                sync.Mutex
	next              int
	cards             []sentCard
	updates           []sentCard
	notices           []string
	receipts          []receiptCall
	files             []string
	downloads         int
	fetches           int
	messages          map[string]core.Message
	historyPage       core.HistoryPage
	historyHook       func(context.Context, core.Room, core.HistoryQuery) (core.HistoryPage, error)
	histories         []core.HistoryQuery
	cardHook          func(context.Context, core.Room, core.Args) (string, error)
	updateHook        func(context.Context, string, core.Args) error
	elementUpdates    []core.CardElementUpdate
	elementUpdateHook func(context.Context, string, []core.CardElementUpdate) error
	downloadHook      func(context.Context, string, core.Resource, string, int64) error
	fetchHook         func(context.Context, string, core.Room) (*core.Message, error)
	fileHook          func(context.Context, string) (string, error)
	receiptHook       func(context.Context, string, string, string) error
}

func (t *fakeTransport) Send(_ context.Context, _ core.Room, text, _, _ string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	t.notices = append(t.notices, text)
	return fmt.Sprintf("notice-%d", t.next), nil
}
func (t *fakeTransport) Card(ctx context.Context, r core.Room, card core.Args, reply, thread string) (string, error) {
	t.mu.Lock()
	t.next++
	id := fmt.Sprintf("card-%d", t.next)
	t.cards = append(t.cards, sentCard{id, r, argsOfCopy(card), reply, thread})
	hook := t.cardHook
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, r, card)
	}
	return id, nil
}
func (t *fakeTransport) Update(ctx context.Context, id string, card core.Args) error {
	t.mu.Lock()
	t.updates = append(t.updates, sentCard{id: id, card: argsOfCopy(card)})
	hook := t.updateHook
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, id, card)
	}
	return nil
}
func (t *fakeTransport) UpdateElements(ctx context.Context, id string, elements []core.CardElementUpdate) error {
	t.mu.Lock()
	t.elementUpdates = append(t.elementUpdates, elements...)
	hook := t.elementUpdateHook
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, id, elements)
	}
	return nil
}
func (t *fakeTransport) Download(ctx context.Context, id string, res core.Resource, path string, limit int64) error {
	t.mu.Lock()
	t.downloads++
	hook := t.downloadHook
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, id, res, path, limit)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = file.WriteString("image bytes"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}
func (t *fakeTransport) Fetch(ctx context.Context, id string, room core.Room) (*core.Message, error) {
	t.mu.Lock()
	t.fetches++
	m, ok := t.messages[id]
	hook := t.fetchHook
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, id, room)
	}
	if !ok {
		return nil, nil
	}
	return &m, nil
}
func (t *fakeTransport) History(ctx context.Context, r core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
	t.mu.Lock()
	t.histories = append(t.histories, q)
	hook := t.historyHook
	page := t.historyPage
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, r, q)
	}
	return page, nil
}
func (t *fakeTransport) Receipt(ctx context.Context, id, desired, applied string) error {
	t.mu.Lock()
	t.receipts = append(t.receipts, receiptCall{id, desired, applied})
	hook := t.receiptHook
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, id, desired, applied)
	}
	return nil
}
func (t *fakeTransport) File(ctx context.Context, _ core.Room, path, _ string) (string, error) {
	t.mu.Lock()
	t.files = append(t.files, path)
	hook := t.fileHook
	t.next++
	id := fmt.Sprintf("file-%d", t.next)
	t.mu.Unlock()
	if hook != nil {
		return hook(ctx, path)
	}
	return id, nil
}
func (t *fakeTransport) cardCount() int { t.mu.Lock(); defer t.mu.Unlock(); return len(t.cards) }
func (t *fakeTransport) cardLast() sentCard {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cards[len(t.cards)-1]
}
func (t *fakeTransport) receiptDone(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.receipts {
		if r.id == id && r.desired == "done" {
			return true
		}
	}
	return false
}
func argsOfCopy(a core.Args) core.Args {
	var out core.Args
	_ = jsonUnmarshal(jsonText(a), &out)
	return out
}
func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

type fakeWorker struct {
	// pendingReady defers the native ready event to the first input, as the
	// real CLI reports its session only once a query starts.
	pendingReady string
	readyMu      sync.Mutex
	contextMu    sync.Mutex
	contextFn    func() *core.RequestContext
	emit         func(core.WorkerEvent)
	sent         chan core.Delivery
	responses    chan core.Args
	renames      chan string
	stops        atomic.Int64
	failStop     atomic.Bool
	stopHook     func(context.Context) error
	stopReason   atomic.Value
	turnActive   atomic.Bool
}

func (w *fakeWorker) SetStopReason(reason string) { w.stopReason.Store(reason) }
func (w *fakeWorker) SetTurnActive(active bool)   { w.turnActive.Store(active) }

func (w *fakeWorker) SetRequestContext(fn func() *core.RequestContext) {
	w.contextMu.Lock()
	w.contextFn = fn
	w.contextMu.Unlock()
}

func (w *fakeWorker) requestContext() *core.RequestContext {
	w.contextMu.Lock()
	fn := w.contextFn
	w.contextMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

func (w *fakeWorker) Send(ctx context.Context, d core.Delivery) error {
	w.readyMu.Lock()
	ready := w.pendingReady
	w.pendingReady = ""
	w.readyMu.Unlock()
	if ready != "" {
		go w.emit(core.WorkerEvent{Type: "ready", CCID: ready})
	}
	select {
	case w.sent <- d:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *fakeWorker) Rename(_ context.Context, title string) error {
	if w.renames != nil {
		w.renames <- title
	}
	return nil
}
func (w *fakeWorker) Respond(_ context.Context, id string, a core.Args) error {
	copy := argsOfCopy(a)
	copy["requestID"] = id
	w.responses <- copy
	return nil
}
func (w *fakeWorker) Stop(ctx context.Context) error {
	w.stops.Add(1)
	if w.stopHook != nil {
		return w.stopHook(ctx)
	}
	if w.failStop.Load() {
		return errors.New("test termination unconfirmed")
	}
	yes := true
	w.emit(core.WorkerEvent{Type: "exit", CleanupConfirmed: &yes})
	return nil
}

type fixture struct {
	t       *testing.T
	c       core.Config
	s       *store.Store
	g       *Gateway
	tr      *fakeTransport
	started chan *fakeWorker
	starts  atomic.Int64
	// readyMode: "" emits ready at start, "input" on the first input for a
	// new session, "never" never reports a native session.
	readyMode      string
	permissionMode string
}

func newFixture(t *testing.T, mutate func(*core.Config), loggers ...core.Logger) *fixture {
	t.Helper()
	var logger core.Logger
	if len(loggers) > 0 {
		logger = loggers[0]
	}
	c := core.Config{TenantID: "tenant", AppID: "app", DataDir: t.TempDir(), Runtime: core.RuntimeConfig{MaxWorkers: 2, IdleMS: 1000}, Model: core.ModelConfig{Model: "test-model"}, Projects: []core.Project{{ID: "project", Maintainers: []string{"owner"}, PermissionMode: "default"}}, Groups: []core.Group{{ChatID: "group", ProjectID: "project", Members: []string{"member", "second"}}, {ChatID: "other", ProjectID: "project", Members: []string{"member"}}}, DMs: []core.DM{{UserID: "member", ProjectID: "project"}, {UserID: "owner", ProjectID: "project"}}, Limits: core.Limits{BackgroundMessages: 0, BackgroundMS: 300000, TextChars: 10000, InboxPerRoom: 20, AttachmentBytes: 1024 * 1024, ApprovalMS: 10000, UpdateMS: 50, MessagesPerMinute: 100, ImageBatchMS: 0, ImageBatchMaxImages: 8, ImageBatchBytes: 1024 * 1024}}
	if mutate != nil {
		mutate(&c)
	}
	st, e := store.Open(filepath.Join(c.DataDir, "state.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{t: t, c: c, s: st, tr: &fakeTransport{messages: map[string]core.Message{}}, started: make(chan *fakeWorker, 20)}
	f.g = New(c, st, f.tr, func(ctx context.Context, r core.Room, s core.Session, w core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		worker := &fakeWorker{emit: emit, sent: make(chan core.Delivery, 100), responses: make(chan core.Args, 100), renames: make(chan string, 8)}
		f.starts.Add(1)
		cc := s.CCID
		if cc == "" {
			cc = core.NewID()
		}
		switch {
		case f.readyMode == "never" && s.CCID == "":
		case f.readyMode == "input" && s.CCID == "":
			worker.pendingReady = cc
		default:
			emit(core.WorkerEvent{Type: "ready", CCID: cc, PermissionMode: f.permissionMode})
		}
		zero := 0
		emit(core.WorkerEvent{Type: "activity", State: "idle", Background: &zero})
		f.started <- worker
		return worker, nil
	}, logger)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureWait)
		defer cancel()
		if e := f.g.Close(ctx); e != nil {
			t.Errorf("close: %v", e)
		}
		if e := f.s.Close(); e != nil {
			t.Error(e)
		}
	})
	return f
}
func (f *fixture) message(id, text, sender string) core.Message {
	return core.Message{ID: id, ChatID: "group", ChatType: "group", SenderID: sender, Text: text, Time: time.Now().UnixMilli(), Mentioned: true, Resources: []core.Resource{}}
}
func (f *fixture) send(m core.Message) {
	f.t.Helper()
	if e := f.g.Message(context.Background(), m); e != nil {
		f.t.Fatal(e)
	}
}
func (f *fixture) worker() *fakeWorker {
	f.t.Helper()
	select {
	case w := <-f.started:
		return w
	case <-time.After(fixtureWait):
		f.t.Fatal("worker not started")
		return nil
	}
}
func (f *fixture) title(w *fakeWorker) string {
	f.t.Helper()
	select {
	case title := <-w.renames:
		return title
	case <-time.After(fixtureWait):
		f.t.Fatal("native title not sent")
		return ""
	}
}
func (f *fixture) delivery(w *fakeWorker) core.Delivery {
	f.t.Helper()
	select {
	case d := <-w.sent:
		return d
	case <-time.After(fixtureWait):
		f.t.Fatal("input not delivered")
		return core.Delivery{}
	}
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	// Generous for race-enabled Windows/macOS CI runners; it only delays a
	// genuine failure and never changes what a passing condition proves.
	deadline := time.Now().Add(fixtureWait)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
func (f *fixture) room() core.Room { r, _ := RoomFor(f.c, "group", ""); return r }
func (f *fixture) status(id string) string {
	var s string
	_ = f.s.DB.QueryRow("SELECT status FROM inbox WHERE id=?", id).Scan(&s)
	return s
}
func (f *fixture) finish(w *fakeWorker, d core.Delivery, text string) {
	w.emit(core.WorkerEvent{Type: "result", UUID: d.UUID, UUIDs: []string{d.UUID}, ResultID: core.NewID(), Text: text, Success: true, Source: "foreground", Usage: core.Args{"input_tokens": int64(3), "output_tokens": int64(2)}})
}
func TestUnauthorizedInputsHaveNoEffects(t *testing.T) {
	f := newFixture(t, nil)
	cases := []core.Message{f.message("bad-author", "secret", "stranger"), f.message("silent", "no mention", "member"), f.message("bot", "text", "member"), f.message("foreign", "text", "member")}
	cases[1].Mentioned = false
	cases[2].SenderIsBot = true
	cases[3].ChatID = "unknown"
	for _, m := range cases {
		m.ReplyTo = "quote"
		m.Resources = []core.Resource{{Key: "k", Kind: "file", Name: "x"}}
		f.send(m)
	}
	var n int
	if e := f.s.DB.QueryRow("SELECT count(*) FROM inbox").Scan(&n); e != nil {
		t.Fatal(e)
	}
	f.tr.mu.Lock()
	effects := f.tr.downloads + f.tr.fetches + len(f.tr.receipts) + len(f.tr.cards) + len(f.tr.notices)
	f.tr.mu.Unlock()
	if n != 0 || effects != 0 || f.starts.Load() != 0 {
		t.Fatalf("rejected input had effects: rows=%d remote=%d starts=%d", n, effects, f.starts.Load())
	}
}
func TestReadCommandsNeverCreateNativeSession(t *testing.T) {
	f := newFixture(t, nil)
	for i, cmd := range []string{"/status --json", "/sessions --json", "/outputs --json", "/usage --json", "/help"} {
		f.send(f.message(fmt.Sprint(i), cmd, "member"))
	}
	eventually(t, func() bool { f.tr.mu.Lock(); defer f.tr.mu.Unlock(); return len(f.tr.notices) == 5 })
	var n int
	_ = f.s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&n)
	if n != 0 || f.starts.Load() != 0 {
		t.Fatal("read command created a session")
	}
}
func TestBusyInputsUseOneWarmWorkerAndDeliveryControlsDone(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "first", "member"))
	w := f.worker()
	d1 := f.delivery(w)
	f.send(f.message("two", "second", "second"))
	d2 := f.delivery(w)
	if f.starts.Load() != 1 || d1.SessionID != d2.SessionID {
		t.Fatal("busy chat started extra worker/session")
	}
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	f.tr.mu.Lock()
	f.tr.cardHook = func(ctx context.Context, _ core.Room, _ core.Args) (string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			return "delivered-card", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.tr.mu.Unlock()
	w.emit(core.WorkerEvent{Type: "result", UUIDs: []string{d1.UUID, d2.UUID}, ResultID: core.NewID(), Success: true, Text: "answer", Source: "foreground"})
	<-started
	if f.tr.receiptDone("one") || f.tr.receiptDone("two") {
		t.Fatal("Done before final provider receipt")
	}
	if f.status("one") != "done" || f.status("two") != "done" {
		t.Fatal("final answer/input completion not durably committed")
	}
	close(gate)
	eventually(t, func() bool { return f.tr.receiptDone("one") && f.tr.receiptDone("two") })
	card := f.tr.cardLast()
	if card.reply != "one" {
		t.Fatalf("lost original reply anchor: %q", card.reply)
	}
}
func TestStopPreservesNativeIdentityAndNewRotates(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "first", "member"))
	w := f.worker()
	d := f.delivery(w)
	var cc string
	_ = f.s.DB.QueryRow("SELECT cc FROM sessions WHERE id=?", d.SessionID).Scan(&cc)
	f.send(f.message("stop", "/stop", "owner"))
	if w.stops.Load() == 0 || f.status("one") != "cancelled" {
		t.Fatal("stop did not cancel current execution")
	}
	f.send(f.message("two", "continue", "member"))
	w2 := f.worker()
	d2 := f.delivery(w2)
	if d2.SessionID != d.SessionID {
		t.Fatal("stop rotated history")
	}
	var cc2 string
	_ = f.s.DB.QueryRow("SELECT cc FROM sessions WHERE id=?", d2.SessionID).Scan(&cc2)
	if cc2 != cc {
		t.Fatal("native identity changed")
	}
	f.send(f.message("new", "/new", "owner"))
	f.send(f.message("three", "fresh", "member"))
	w3 := f.worker()
	d3 := f.delivery(w3)
	if d3.SessionID == d.SessionID {
		t.Fatal("explicit new failed to rotate")
	}
}
func TestStopBypassesRateLimitButMembersCannotStop(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Limits.MessagesPerMinute = 1 })
	f.send(f.message("one", "first", "owner"))
	w := f.worker()
	_ = f.delivery(w)
	f.send(f.message("denied", "/stop", "member"))
	if w.stops.Load() != 0 {
		t.Fatal("member stopped shared worker")
	}
	f.send(f.message("stop", "/stop", "owner"))
	if w.stops.Load() != 1 {
		t.Fatal("rate limit blocked authorized stop")
	}
}
func TestSupplementIsFencedToExistingForeground(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("none", "/btw nothing", "member"))
	if f.starts.Load() != 0 {
		t.Fatal("supplement started new worker")
	}
	f.send(f.message("one", "first", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.send(f.message("supp", "/ps /stop is literal supplemental text", "member"))
	supp := f.delivery(w)
	if supp.SupplementTo != d.UUID || supp.Message.Text != "/stop is literal supplemental text" || w.stops.Load() != 0 {
		t.Fatal("supplement not attached or reparsed as command")
	}
	w.emit(core.WorkerEvent{Type: "input_rejected", UUID: supp.UUID, Reason: "foreground_ended"})
	eventually(t, func() bool { return f.status("supp") == "cancelled" })
	f.finish(w, d, "finished")
	f.send(f.message("after", "/btw no new run", "member"))
	if f.status("after") != "cancelled" {
		t.Fatal("late supplement was accepted")
	}
}
func TestCompactWaitsForTurnAndKeepsApprovals(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "first", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "permission", Kind: "permission", Name: "Bash", Input: core.Args{"command": "example"}})
	eventually(t, func() bool { return f.tr.cardCount() == 1 })
	approvalCard := f.tr.cardLast()
	f.send(f.message("compact", "/compact preserve goals", "owner"))
	select {
	case <-w.sent:
		t.Fatal("compact sent during active turn")
	default:
	}
	f.finish(w, d, "done")
	time.Sleep(30 * time.Millisecond)
	select {
	case <-w.sent:
		t.Fatal("compact evicted outstanding approval")
	default:
	}
	card := approvalCard
	id := stringArg(cardValue(card.card, "拒绝"), "request")
	msg, e := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: core.Args{"request": id, "decision": "deny"}})
	if e != nil || msg != "已拒绝" {
		t.Fatalf("deny: %q %v", msg, e)
	}
	compact := f.delivery(w)
	if compact.Command == nil || compact.Command.Name != "compact" || compact.Command.Instructions != "preserve goals" || compact.SessionID != d.SessionID {
		t.Fatal("native compact semantics changed")
	}
	f.finish(w, compact, "")
}
func TestStopCancelsSlowDownloadWithoutWaitingForProvider(t *testing.T) {
	f := newFixture(t, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	f.tr.downloadHook = func(_ context.Context, _ string, _ core.Resource, path string, _ int64) error {
		close(started)
		<-release
		return os.WriteFile(path, []byte("late"), 0600)
	}
	m := f.message("image", "look", "member")
	m.Resources = []core.Resource{{Key: "img", Kind: "image", Name: "image.png"}}
	f.send(m)
	w := f.worker()
	<-started
	done := make(chan error, 1)
	go func() { done <- f.g.Message(context.Background(), f.message("stop", "/stop", "owner")) }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("stop blocked on download")
	}
	close(release)
	if f.status("image") != "cancelled" {
		t.Fatal("cancelled preparation not persisted")
	}
	select {
	case <-w.sent:
		t.Fatal("input sent after stop")
	default:
	}
}
func TestUnknownCardCreateRequiresExplicitRetryAndRoomOwnership(t *testing.T) {
	f := newFixture(t, nil)
	ctx := f.deliveryScenarioContext()
	done := f.doneReceiptEvent("one")
	f.tr.cardHook = func(context.Context, core.Room, core.Args) (string, error) {
		return "", errors.New("lost create response")
	}
	f.send(f.message("one", "first", "member"))
	w := awaitFixtureEvent(f, ctx, "worker factory and startup events", f.started)
	d := awaitFixtureEvent(f, ctx, "input delivered to worker", w.sent)
	f.finish(w, d, "durable answer")
	var id, state string
	f.awaitDeliveryState(ctx, "unknown create persisted as uncertain", func(ctx context.Context) (bool, error) {
		err := f.s.DB.QueryRowContext(ctx, "SELECT id,state FROM outbox WHERE kind='reply'").Scan(&id, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return state == "uncertain", err
	})
	before := f.tr.cardCount()
	f.g.Sweep(ctx, time.Now().UnixMilli())
	time.Sleep(30 * time.Millisecond)
	if f.tr.cardCount() != before {
		t.Fatal("uncertain create replayed automatically")
	}
	other, _ := RoomFor(f.c, "other", "")
	if ok, e := f.g.deliverOutput(ctx, id, other, true); e != nil || ok {
		t.Fatal("foreign room replayed output")
	}
	if f.tr.receiptDone("one") {
		t.Fatal("uncertain output received Done before explicit retry")
	}
	f.tr.mu.Lock()
	f.tr.cardHook = nil
	f.tr.mu.Unlock()
	ok, e := f.g.deliverOutput(ctx, id, f.room(), true)
	if e != nil || !ok {
		t.Fatalf("explicit retry: %t %v", ok, e)
	}
	awaitFixtureEvent(f, ctx, "explicit retry Done receipt", done)
}

func TestNativeSessionTitleUsesFirstMessageAndRename(t *testing.T) {
	f := newFixture(t, nil)
	m := f.message("title-source", "第一行\n第二行", "member")
	m.SenderName = "张三"
	f.send(m)
	w := f.worker()
	d := f.delivery(w)
	if d.Message.ID != "title-source" {
		t.Fatalf("delivered %q", d.Message.ID)
	}
	if title := f.title(w); title != "张三：第一行 第二行" {
		t.Fatalf("first native title %q", title)
	}
	f.finish(w, d, "done")
	f.send(f.message("rename", "/rename 发布检查", "owner"))
	if title := f.title(w); title != "发布检查" {
		t.Fatalf("renamed native title %q", title)
	}
	f.send(f.message("clear-name", "/rename -", "owner"))
	if title := f.title(w); title != "张三：第一行 第二行" {
		t.Fatalf("cleared native title %q", title)
	}
}
