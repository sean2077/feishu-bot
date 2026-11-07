package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

type classifiedFailure struct {
	retryable, uncertain bool
	after                time.Duration
}

func (e classifiedFailure) Error() string                     { return "test delivery rejection" }
func (e classifiedFailure) DeliveryRetryable() bool           { return e.retryable }
func (e classifiedFailure) DeliveryUncertain() bool           { return e.uncertain }
func (e classifiedFailure) DeliveryRetryAfter() time.Duration { return e.after }

func TestReplyBudgetUsesSerializedBytesIncludingMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		truncated  bool
	}{
		{"short", "中文与 emoji 🐱", false},
		{"chinese", strings.Repeat("中", 12000), true},
		{"emoji", strings.Repeat("🧑‍💻", 5000), true},
		{"escaped", strings.Repeat("\"\\\n<>&", 5000), true},
		{"code", strings.Repeat("```go\nfmt.Println(\"中文\")\n```\n", 3000), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, final := range []bool{false, true} {
				card, truncated := boundedReplyCard("回复", tc.text, []string{strings.Repeat("思考🧑‍💻", 100)}, 1, final,
					replyDetails{Model: strings.Repeat("模型", 300), ProjectPath: strings.Repeat("/项目", 1000)}, time.Second, true,
					[]any{button("状态", core.Args{"id": strings.Repeat("a", 64)}, "")})
				n, err := core.CardPayloadBytes(card)
				if err != nil || n > core.CardPayloadLimit {
					t.Fatalf("wire budget: %d %v", n, err)
				}
				if truncated != tc.truncated {
					t.Fatalf("truncated=%t", truncated)
				}
				if !utf8.ValidString(jsonText(card)) {
					t.Fatal("split Unicode code point")
				}
			}
		})
	}
}

func TestChineseFinalExportsFullAnswerBeforeDone(t *testing.T) {
	f := newFixture(t, nil)
	answer := strings.Repeat("中文🧑‍💻\"\\\n", 5000)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.tr.fileHook = func(ctx context.Context, path string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != answer {
			t.Errorf("complete export differs: %v", err)
		}
		close(started)
		select {
		case <-release:
			return "full-answer-file", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.send(f.message("long", "write a long reply", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, answer)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("full answer file not sent")
	}
	if f.tr.receiptDone("long") {
		t.Fatal("Done before file acknowledgement")
	}
	card := f.tr.cardLast()
	n, err := core.CardPayloadBytes(card.card)
	if err != nil || n > core.CardPayloadLimit {
		t.Fatalf("oversized final: %d %v", n, err)
	}
	if !strings.Contains(jsonText(card.card), "完整答案另附文件") {
		t.Fatal("missing export notice")
	}
	once.Do(func() { close(release) })
	eventually(t, func() bool { return f.tr.receiptDone("long") })
	if f.tr.cardCount() != 1 {
		t.Fatal("long reply created multiple cards")
	}
}

func TestDeliveryClassificationsControlRetryState(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		err         error
	}{
		{"transient", "pending", classifiedFailure{retryable: true, after: time.Minute}},
		{"permanent", "blocked", classifiedFailure{}},
		{"uncertain", "uncertain", classifiedFailure{uncertain: true}},
		{"unknown", "uncertain", errors.New("response lost")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.tr.cardHook = func(context.Context, core.Room, core.Args) (string, error) { return "", tc.err }
			id, err := f.g.saveOutput(f.s.DB, f.room(), "session", "answer", true, "", "", "", DisplayCard("回复", "answer"), nil, "", "", "reply", "pending")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); ok || err == nil {
				t.Fatal("rejection reported delivered")
			}
			o, err := f.g.output(id)
			if err != nil || o.State != tc.state {
				t.Fatalf("state=%s err=%v", o.State, err)
			}
			if tc.name == "uncertain" {
				var reason string
				if err := f.s.DB.QueryRow("SELECT delivery_error FROM outbox WHERE id=?", id).Scan(&reason); err != nil || !strings.Contains(reason, "unconfirmed") || !strings.Contains(reason, tc.err.Error()) {
					t.Fatalf("lost safe ambiguity diagnostics: %q %v", reason, err)
				}
			}
			if tc.state == "pending" && o.RetryAfter < time.Now().Add(50*time.Second).UnixMilli() {
				t.Fatal("Retry-After discarded")
			}
			before := f.tr.cardCount()
			_, _ = f.g.deliverOutput(context.Background(), id, f.room(), false)
			if f.tr.cardCount() != before {
				t.Fatal("retry bypassed uncertainty, rejection or backoff")
			}
		})
	}
}

func TestPreviewRetriesWithoutAnotherDeltaAndKeepsCreateIdentity(t *testing.T) {
	f := newFixture(t, nil)
	var calls atomic.Int64
	var mu sync.Mutex
	var identities, snapshots []string
	f.tr.cardHook = func(ctx context.Context, _ core.Room, card core.Args) (string, error) {
		mu.Lock()
		identities = append(identities, core.DeliveryID(ctx))
		snapshots = append(snapshots, jsonText(card))
		mu.Unlock()
		if calls.Add(1) == 1 {
			return "", classifiedFailure{retryable: true}
		}
		return "confirmed-preview", nil
	}
	f.send(f.message("preview", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "text", TurnID: d.UUID, BlockID: "text", Text: "first", Partial: true})
	eventually(t, func() bool { return calls.Load() >= 2 })
	mu.Lock()
	if identities[0] == "" || identities[0] != identities[1] || snapshots[0] != snapshots[1] {
		t.Error("logical create changed across retry")
	}
	mu.Unlock()
	f.finish(w, d, "finished")
	eventually(t, func() bool { return f.tr.receiptDone("preview") })
	if calls.Load() != 2 {
		t.Fatal("final reply created another card")
	}
}

func TestPausedReplyResumesSameCardAndStopClosesDurably(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("pause", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type: "text", TurnID: d.UUID, BlockID: "text", Text: "partial", Partial: true})
	eventually(t, func() bool { return f.tr.cardCount() == 1 })
	r := f.g.renderFor(f.g.getSlot(f.room().Key), d.UUID, "", "")
	r.waitForAnswer(true, "等待回答")
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		return len(f.tr.updates) > 0 && argsOf(f.tr.updates[len(f.tr.updates)-1].card["config"])["streaming_mode"] == false
	})
	r.waitForAnswer(false, "")
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		return len(f.tr.updates) > 0 && argsOf(f.tr.updates[len(f.tr.updates)-1].card["config"])["streaming_mode"] == true
	})
	f.send(f.message("stop-pause", "/stop", "owner"))
	eventually(t, func() bool {
		var n int
		_ = f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='status' AND state='done'").Scan(&n)
		return n == 1
	})
	if f.tr.cardCount() != 1 {
		t.Fatal("pause or stop created replacement card")
	}
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	last := f.tr.updates[len(f.tr.updates)-1]
	if argsOf(last.card["config"])["streaming_mode"] != false || !strings.Contains(jsonText(last.card), "已停止") {
		t.Fatal("old card left streaming")
	}
}

func TestRenderReplacementsAndCrossDeltaRedaction(t *testing.T) {
	f := newFixture(t, nil)
	f.g.secrets = []string{"CROSS_DELTA_SECRET"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &render{g: f.g, ctx: ctx, blocks: map[string]*renderBlock{}, thinking: map[string]string{}, closed: false}
	r.update("CROSS_DELTA_", true, "text", "a")
	r.update("SECRET", true, "text", "a")
	r.mu.Lock()
	if strings.Contains(r.answerLocked(), "CROSS_DELTA_SECRET") {
		t.Error("cross-delta secret leaked")
	}
	r.mu.Unlock()
	r.update("替换🧑‍💻", false, "text", "a")
	r.update("新增", true, "text", "a")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	if r.answerLocked() != "替换🧑‍💻新增" || r.totalChars != utf8.RuneCountInString("替换🧑‍💻新增") {
		t.Fatal("replacement duplicated old block or character count")
	}
}

func TestPendingIndependentFileDoesNotMarkReplyTerminal(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "", true, "", "", "", nil, nil, "attachment.txt", filepath.Join(f.c.DataDir, "exports", "pending"), "file", "pending")
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "answer", true, "", "", "", replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true), nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	var terminal atomic.Bool
	f.tr.updateHook = func(ctx context.Context, _ string, _ core.Args) error {
		if core.IsTerminalCard(ctx) {
			terminal.Store(true)
		}
		return nil
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); ok || err != nil {
		t.Fatal("reply completed before independent file", ok, err)
	}
	if terminal.Load() {
		t.Fatal("waiting card marked terminal")
	}
	if _, err := f.s.DB.Exec("UPDATE outbox SET state='done',file_state='done',file_target='file-confirmed' WHERE id=?", file); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if !terminal.Load() {
		t.Fatal("confirmed final card not terminal")
	}
}

func TestFailedFileNoticeKeepsWorkingOutputRetryControls(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	f.tr.fileHook = func(context.Context, string) (string, error) { return "", classifiedFailure{uncertain: true} }
	path := filepath.Join(f.c.DataDir, "exports", core.Hash(f.room().Key)[:32], "reply.md")
	card := replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true)
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "answer", true, "", "", "", card, nil, "reply.md", path, "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); ok || err == nil {
		t.Fatal("unknown file marked done")
	}
	f.tr.mu.Lock()
	last := f.tr.updates[len(f.tr.updates)-1]
	f.tr.mu.Unlock()
	value := testQuickValue(t, f.g, last.card, "retry")
	p, ok := f.g.readQuick(stringArg(value, "quick_control"))
	action := core.CardAction{ChatID: f.room().ChatID, MessageID: last.id, UserID: "owner", Value: value}
	if !ok || p.Output != id || !f.g.quickBound(context.Background(), action, p) {
		t.Fatal("visible retry control lost output binding")
	}
	f.tr.mu.Lock()
	f.tr.fileHook = nil
	f.tr.mu.Unlock()
	if _, err := f.g.Action(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { o, e := f.g.output(id); return e == nil && o.State == "done" })
}

func TestFinalBeforeReservedPreviewFlushKeepsOriginalCreate(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("race", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	r := newRender(f.g, s, &d)
	r.flushing = true
	r.done = make(chan struct{})
	var sentKey, sentSnapshot string
	f.tr.cardHook = func(ctx context.Context, _ core.Room, card core.Args) (string, error) {
		sentKey = core.DeliveryID(ctx)
		sentSnapshot = jsonText(card)
		return "", classifiedFailure{uncertain: true}
	}
	drain, err := r.persist("final answer", true, origin(d.Message), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.initialCard == nil {
		t.Fatal("reserved initial create not frozen by final persistence")
	}
	r.flush()
	drain()
	o, err := f.g.output(r.finalID)
	if err != nil || o.State != "uncertain" || o.DeliveryKey != sentKey || jsonText(o.InitialCard) != sentSnapshot {
		t.Fatalf("lost original create identity: %s %s %v", o.State, o.DeliveryKey, err)
	}
	f.tr.mu.Lock()
	f.tr.cardHook = func(ctx context.Context, _ core.Room, card core.Args) (string, error) {
		if core.DeliveryID(ctx) != sentKey || jsonText(card) != sentSnapshot {
			t.Error("explicit recovery changed original create")
		}
		return "original-message", nil
	}
	f.tr.mu.Unlock()
	if ok, err := f.g.deliverOutput(context.Background(), o.ID, f.room(), true); !ok || err != nil {
		t.Fatal(ok, err)
	}
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	if !strings.Contains(jsonText(f.tr.updates[len(f.tr.updates)-1].card), "final answer") {
		t.Fatal("final snapshot not applied to recovered message")
	}
}

func TestLegacyReplyBudgetIncludesNewControlsAndExportsAllText(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	answer := strings.Repeat("a", 27000)
	card := DisplayCard("回复", answer)
	if n, err := core.CardPayloadBytes(card); err != nil || n > core.CardPayloadLimit {
		t.Fatal("fixture was already oversized", n, err)
	}
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, answer, true, "", "", "", card, nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); !ok || err != nil {
		t.Fatal(ok, err)
	}
	o, err := f.g.output(id)
	if err != nil || o.FilePath == "" || o.FileState != "done" {
		t.Fatal("controls silently truncated the only answer", o, err)
	}
	data, err := os.ReadFile(o.FilePath)
	if err != nil || string(data) != answer {
		t.Fatal("legacy complete answer lost", err)
	}
	if f.starts.Load() != 0 {
		t.Fatal("migration woke the model")
	}
}

func TestExplicitKnownTargetRetryHonorsProviderDeadline(t *testing.T) {
	f := newFixture(t, nil)
	id, err := f.g.saveOutput(f.s.DB, f.room(), "session", "answer", true, "known-card", "", "", DisplayCard("回复", "answer"), nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec("UPDATE outbox SET retry_after=? WHERE id=?", time.Now().Add(time.Minute).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), true); ok || err != nil {
		t.Fatal(ok, err)
	}
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	if len(f.tr.updates) != 0 {
		t.Fatal("explicit retry ignored provider Retry-After")
	}
}

func TestDefinitePreviewCreateRejectionDoesNotFreezePreviewIdentity(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("reject-preview", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	r := newRender(f.g, s, &d)
	f.tr.cardHook = func(context.Context, core.Room, core.Args) (string, error) { return "", classifiedFailure{} }
	r.flushing = true
	r.done = make(chan struct{})
	r.flush()
	if !r.createRejected || r.initialCard == nil {
		t.Fatalf("definite preview rejection was not tracked: rejected=%t initial=%t", r.createRejected, r.initialCard != nil)
	}
	drain, err := r.persist("final answer", true, origin(d.Message), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.g.output(r.finalID)
	if err != nil {
		t.Fatal(err)
	}
	if o.InitialCard != nil || o.DeliveryKey != "output:"+o.ID {
		t.Fatalf("rejected preview identity was frozen into the final delivery: %+v", o)
	}
	var delivered core.Args
	f.tr.mu.Lock()
	f.tr.cardHook = func(_ context.Context, _ core.Room, card core.Args) (string, error) {
		delivered = card
		return "om-final", nil
	}
	f.tr.mu.Unlock()
	drain()
	if !strings.Contains(jsonText(delivered), "final answer") {
		t.Fatalf("final card was not created from the final answer: %s", jsonText(delivered))
	}
	if done, err := f.g.output(r.finalID); err != nil || done.State != "done" {
		t.Fatalf("final delivery state: %+v %v", done, err)
	}
}

func TestFileCompletingDuringCardUpdateStillDefersReply(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.c.DataDir, "exports", "attachment.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "", true, "", "", "", nil, nil, "attachment.txt", path, "file", "pending")
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "answer", true, "", "", "", replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true), nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	f.tr.updateHook = func(context.Context, string, core.Args) error {
		_, e := f.s.DB.Exec("UPDATE outbox SET state='done',file_state='done',file_target='file-confirmed' WHERE id=?", file)
		return e
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); ok || err != nil {
		t.Fatalf("reply delivery before attachment acknowledgement: ok=%t err=%v", ok, err)
	}
	if pending, err := f.g.output(id); err != nil || pending.State != "pending" {
		t.Fatalf("reply finalized while its card announced a pending attachment: %+v %v", pending, err)
	}
	f.tr.updateHook = nil
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); !ok || err != nil {
		t.Fatalf("reply did not finalize after attachment acknowledgement: ok=%t err=%v", ok, err)
	}
	if done, err := f.g.output(id); err != nil || done.State != "done" {
		t.Fatalf("reply state after attachment acknowledgement: %+v %v", done, err)
	}
}

func TestLegacyOversizedMetadataStillDeliversTheCompleteAnswer(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	answer := strings.Repeat("完整正文", 5000)
	card := DisplayCard(strings.Repeat("旧标题", 20000), answer)
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, answer, true, "", "", "", card, nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); !ok || err != nil {
		t.Fatal(ok, err)
	}
	o, err := f.g.output(id)
	if err != nil || o.State != "done" || o.FileState != "done" {
		t.Fatal("oversized metadata blocked the full answer", err)
	}
	data, err := os.ReadFile(o.FilePath)
	if err != nil || string(data) != answer {
		t.Fatal("legacy answer file lost content", err)
	}
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	for _, update := range f.tr.updates {
		if size, err := core.CardPayloadBytes(update.card); err != nil || size > core.CardPayloadLimit {
			t.Fatal("oversized fallback", size, err)
		}
	}
	if f.starts.Load() != 0 {
		t.Fatal("delivery woke the model")
	}
}

func TestBlockedAttachmentFromEarlierTurnDoesNotDeferLaterReply(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.c.DataDir, "exports", "old.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.saveOutput(f.s.DB, r, s.ID, "", true, "", "", "", nil, []core.Origin{{ID: "old-message", SenderID: "member"}}, "old.txt", path, "file", "blocked"); err != nil {
		t.Fatal(err)
	}
	card := replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true)
	id, err := f.g.saveOutput(f.s.DB, r, s.ID, "answer", true, "", "", "", card, []core.Origin{{ID: "new-message", SenderID: "member"}}, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range f.g.retryable(r.Key) {
		found = found || candidate == id
	}
	if !found {
		t.Fatal("reply stayed filtered by an earlier turn's blocked attachment")
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, r, false); !ok || err != nil {
		t.Fatalf("later reply delivery: ok=%t err=%v", ok, err)
	}
	if done, err := f.g.output(id); err != nil || done.State != "done" {
		t.Fatalf("later reply state: %+v %v", done, err)
	}
}

func TestPendingAttachmentFromSameTurnDefersReply(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.c.DataDir, "exports", "same.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("same"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.saveOutput(f.s.DB, r, s.ID, "", true, "", "", "", nil, []core.Origin{{ID: "same-message", SenderID: "member"}}, "same.txt", path, "file", "pending"); err != nil {
		t.Fatal(err)
	}
	card := replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true)
	id, err := f.g.saveOutput(f.s.DB, r, s.ID, "answer", true, "", "", "", card, []core.Origin{{ID: "same-message", SenderID: "member"}}, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, r, false); ok || err != nil {
		t.Fatalf("same-turn attachment did not defer reply: ok=%t err=%v", ok, err)
	}
	if pending, err := f.g.output(id); err != nil || pending.State != "pending" {
		t.Fatalf("reply state: %+v %v", pending, err)
	}
}

func TestApprovalWaitRefreshesCardAfterBlockedPreview(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("blocked-preview", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	r := newRender(f.g, s, &d)
	r.messageID = "om_card"
	r.last = jsonText(r.cardLocked("处理中", "", false, false))
	r.blocked = true
	r.waitForAnswer(true, "等待审批")
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, update := range f.tr.updates {
			if update.id == "om_card" && strings.Contains(jsonText(update.card), "等待审批") {
				return true
			}
		}
		return false
	})
}

func TestSuccessfulTextReplyDropsRetryControl(t *testing.T) {
	f := newFixture(t, nil)
	room := f.room()
	s, err := f.s.Session(room.Key)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.g.saveOutput(f.s.DB, room, s.ID, "answer", true, "", "", "", replyCard("回复", "answer", nil, 0, true, replyDetails{}, time.Second, true), nil, "", "", "reply", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.g.deliverOutput(context.Background(), id, room, false); !ok || err != nil {
		t.Fatalf("delivery: %t %v", ok, err)
	}
	f.tr.mu.Lock()
	if len(f.tr.updates) < 2 {
		f.tr.mu.Unlock()
		t.Fatalf("expected delivery and terminal control updates, got %d", len(f.tr.updates))
	}
	visible := jsonText(f.tr.updates[len(f.tr.updates)-1].card)
	f.tr.mu.Unlock()
	if !strings.Contains(visible, "查看状态") || !strings.Contains(visible, "待回答") {
		t.Fatalf("terminal card lost session actions: %s", visible)
	}
	if strings.Contains(visible, "重试交付") {
		t.Fatalf("terminal card still offers retry: %s", visible)
	}
	var state string
	if err := f.s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&state); err != nil || state != "done" {
		t.Fatalf("outbox state = %q, %v", state, err)
	}
}
