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

type classifiedFailure struct { retryable, uncertain bool; after time.Duration }
func (e classifiedFailure) Error() string { return "test delivery rejection" }
func (e classifiedFailure) DeliveryRetryable() bool { return e.retryable }
func (e classifiedFailure) DeliveryUncertain() bool { return e.uncertain }
func (e classifiedFailure) DeliveryRetryAfter() time.Duration { return e.after }

func TestReplyBudgetUsesSerializedBytesIncludingMetadata(t *testing.T) {
	for _, tc := range []struct { name, text string; truncated bool }{
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
				if err != nil || n > core.CardPayloadLimit { t.Fatalf("wire budget: %d %v", n, err) }
				if truncated != tc.truncated { t.Fatalf("truncated=%t", truncated) }
				if !utf8.ValidString(jsonText(card)) { t.Fatal("split Unicode code point") }
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
		if err != nil || string(data) != answer { t.Errorf("complete export differs: %v", err) }
		close(started)
		select { case <-release: return "full-answer-file", nil; case <-ctx.Done(): return "", ctx.Err() }
	}
	f.send(f.message("long", "write a long reply", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, answer)
	select { case <-started: case <-time.After(3*time.Second): t.Fatal("full answer file not sent") }
	if f.tr.receiptDone("long") { t.Fatal("Done before file acknowledgement") }
	card := f.tr.cardLast()
	n, err := core.CardPayloadBytes(card.card)
	if err != nil || n > core.CardPayloadLimit { t.Fatalf("oversized final: %d %v", n, err) }
	if !strings.Contains(jsonText(card.card), "完整答案另附文件") { t.Fatal("missing export notice") }
	once.Do(func() { close(release) })
	eventually(t, func() bool { return f.tr.receiptDone("long") })
	if f.tr.cardCount() != 1 { t.Fatal("long reply created multiple cards") }
}

func TestDeliveryClassificationsControlRetryState(t *testing.T) {
	for _, tc := range []struct { name, state string; err error }{
		{"transient", "pending", classifiedFailure{retryable:true, after: time.Minute}},
		{"permanent", "blocked", classifiedFailure{}},
		{"uncertain", "uncertain", classifiedFailure{uncertain:true}},
		{"unknown", "uncertain", errors.New("response lost")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.tr.cardHook = func(context.Context, core.Room, core.Args) (string, error) { return "", tc.err }
			id, err := f.g.saveOutput(f.s.DB, f.room(), "session", "answer", true, "", "", "", DisplayCard("回复", "answer"), nil, "", "", "reply", "pending")
			if err != nil { t.Fatal(err) }
			if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); ok || err == nil { t.Fatal("rejection reported delivered") }
			o, err := f.g.output(id)
			if err != nil || o.State != tc.state { t.Fatalf("state=%s err=%v", o.State, err) }
			if tc.state == "pending" && o.RetryAfter < time.Now().Add(50*time.Second).UnixMilli() { t.Fatal("Retry-After discarded") }
			before := f.tr.cardCount()
			_, _ = f.g.deliverOutput(context.Background(), id, f.room(), false)
			if f.tr.cardCount() != before { t.Fatal("retry bypassed uncertainty, rejection or backoff") }
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
		if calls.Add(1) == 1 { return "", classifiedFailure{retryable:true} }
		return "confirmed-preview", nil
	}
	f.send(f.message("preview", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type:"text", TurnID:d.UUID, BlockID:"text", Text:"first", Partial:true})
	eventually(t, func() bool { return calls.Load() >= 2 })
	mu.Lock()
	if identities[0] == "" || identities[0] != identities[1] || snapshots[0] != snapshots[1] { t.Error("logical create changed across retry") }
	mu.Unlock()
	f.finish(w, d, "finished")
	eventually(t, func() bool { return f.tr.receiptDone("preview") })
	if calls.Load() != 2 { t.Fatal("final reply created another card") }
}

func TestPausedReplyResumesSameCardAndStopClosesDurably(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("pause", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	w.emit(core.WorkerEvent{Type:"text", TurnID:d.UUID, BlockID:"text", Text:"partial", Partial:true})
	eventually(t, func() bool { return f.tr.cardCount() == 1 })
	r := f.g.renderFor(f.g.getSlot(f.room().Key), d.UUID, "", "")
	r.waitForAnswer(true, "等待回答")
	eventually(t, func() bool {
		f.tr.mu.Lock(); defer f.tr.mu.Unlock()
		return len(f.tr.updates)>0 && argsOf(f.tr.updates[len(f.tr.updates)-1].card["config"])["streaming_mode"] == false
	})
	r.waitForAnswer(false, "")
	eventually(t, func() bool {
		f.tr.mu.Lock(); defer f.tr.mu.Unlock()
		return len(f.tr.updates)>0 && argsOf(f.tr.updates[len(f.tr.updates)-1].card["config"])["streaming_mode"] == true
	})
	f.send(f.message("stop-pause", "/stop", "owner"))
	eventually(t, func() bool {
		var n int
		_ = f.s.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind='status' AND state='done'").Scan(&n)
		return n == 1
	})
	if f.tr.cardCount() != 1 { t.Fatal("pause or stop created replacement card") }
	f.tr.mu.Lock(); defer f.tr.mu.Unlock()
	last := f.tr.updates[len(f.tr.updates)-1]
	if argsOf(last.card["config"])["streaming_mode"] != false || !strings.Contains(jsonText(last.card), "已停止") { t.Fatal("old card left streaming") }
}

func TestRenderReplacementsAndCrossDeltaRedaction(t *testing.T) {
	f := newFixture(t, nil)
	f.g.secrets = []string{"CROSS_DELTA_SECRET"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &render{g:f.g, ctx:ctx, blocks:map[string]*renderBlock{}, thinking:map[string]string{}, closed:false}
	r.update("CROSS_DELTA_", true, "text", "a")
	r.update("SECRET", true, "text", "a")
	r.mu.Lock()
	if strings.Contains(r.answerLocked(), "CROSS_DELTA_SECRET") { t.Error("cross-delta secret leaked") }
	r.mu.Unlock()
	r.update("替换🧑‍💻", false, "text", "a")
	r.update("新增", true, "text", "a")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil { r.timer.Stop() }
	if r.answerLocked() != "替换🧑‍💻新增" || r.totalChars != utf8.RuneCountInString("替换🧑‍💻新增") { t.Fatal("replacement duplicated old block or character count") }
}

func TestPendingIndependentFileDoesNotMarkReplyTerminal(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil { t.Fatal(err) }
	file, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "", true, "", "", "", nil, nil, "attachment.txt", filepath.Join(f.c.DataDir,"exports","pending"), "file", "pending")
	if err != nil { t.Fatal(err) }
	id, err := f.g.saveOutput(f.s.DB, f.room(), s.ID, "answer", true, "", "", "", replyCard("回复","answer",nil,0,true,replyDetails{},time.Second,true), nil, "", "", "reply", "pending")
	if err != nil { t.Fatal(err) }
	var terminal atomic.Bool
	f.tr.updateHook = func(ctx context.Context, _ string, _ core.Args) error { if core.IsTerminalCard(ctx) { terminal.Store(true) }; return nil }
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); ok || err != nil { t.Fatal("reply completed before independent file",ok,err) }
	if terminal.Load() { t.Fatal("waiting card marked terminal") }
	if _, err := f.s.DB.Exec("UPDATE outbox SET state='done',file_state='done',file_target='file-confirmed' WHERE id=?",file);err!=nil { t.Fatal(err) }
	if ok, err := f.g.deliverOutput(context.Background(), id, f.room(), false); !ok || err != nil { t.Fatal(ok,err) }
	if !terminal.Load() { t.Fatal("confirmed final card not terminal") }
}

func TestFailedFileNoticeKeepsWorkingOutputRetryControls(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.s.Session(f.room().Key)
	if err != nil { t.Fatal(err) }
	f.tr.fileHook = func(context.Context,string)(string,error) { return "", classifiedFailure{uncertain:true} }
	path := filepath.Join(f.c.DataDir,"exports",core.Hash(f.room().Key)[:32],"reply.md")
	card := replyCard("回复","answer",nil,0,true,replyDetails{},time.Second,true)
	id, err := f.g.saveOutput(f.s.DB,f.room(),s.ID,"answer",true,"","","",card,nil,"reply.md",path,"reply","pending")
	if err != nil { t.Fatal(err) }
	if ok, err := f.g.deliverOutput(context.Background(),id,f.room(),false); ok || err==nil { t.Fatal("unknown file marked done") }
	f.tr.mu.Lock()
	last := f.tr.updates[len(f.tr.updates)-1]
	f.tr.mu.Unlock()
	value := testQuickValue(t,f.g,last.card,"retry")
	p, ok := f.g.readQuick(stringArg(value,"quick_control"))
	action := core.CardAction{ChatID:f.room().ChatID,MessageID:last.id,UserID:"owner",Value:value}
	if !ok || p.Output!=id || !f.g.quickBound(action,p) { t.Fatal("visible retry control lost output binding") }
	f.tr.mu.Lock();f.tr.fileHook=nil;f.tr.mu.Unlock()
	if _, err := f.g.Action(context.Background(),action);err!=nil { t.Fatal(err) }
	eventually(t,func()bool { o,e:=f.g.output(id);return e==nil && o.State=="done" })
}

func TestFinalBeforeReservedPreviewFlushKeepsOriginalCreate(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("race", "hello", "member"))
	w := f.worker()
	d := f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	r := newRender(f.g,s,&d)
	r.flushing=true
	r.done=make(chan struct{})
	var sentKey, sentSnapshot string
	f.tr.cardHook=func(ctx context.Context,_ core.Room,card core.Args)(string,error){ sentKey=core.DeliveryID(ctx);sentSnapshot=jsonText(card);return "",classifiedFailure{uncertain:true} }
	drain,err:=r.persist("final answer",true,origin(d.Message),"",nil)
	if err!=nil {t.Fatal(err)}
	if r.initialCard==nil {t.Fatal("reserved initial create not frozen by final persistence")}
	r.flush()
	drain()
	o,err:=f.g.output(r.finalID)
	if err!=nil || o.State!="uncertain" || o.DeliveryKey!=sentKey || jsonText(o.InitialCard)!=sentSnapshot { t.Fatalf("lost original create identity: %s %s %v",o.State,o.DeliveryKey,err) }
	f.tr.mu.Lock(); f.tr.cardHook=func(ctx context.Context,_ core.Room,card core.Args)(string,error){
		if core.DeliveryID(ctx)!=sentKey || jsonText(card)!=sentSnapshot {t.Error("explicit recovery changed original create")}
		return "original-message",nil
	}; f.tr.mu.Unlock()
	if ok,err:=f.g.deliverOutput(context.Background(),o.ID,f.room(),true);!ok||err!=nil {t.Fatal(ok,err)}
	f.tr.mu.Lock();defer f.tr.mu.Unlock()
	if !strings.Contains(jsonText(f.tr.updates[len(f.tr.updates)-1].card),"final answer") {t.Fatal("final snapshot not applied to recovered message")}
}
