package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/gateway"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/store"
)

type observedCard struct {
	id, replyTo string
	room        core.Room
	card        core.Args
}

type deliveredFile struct {
	name string
	text string
}

type localFeishu struct {
	mu          sync.Mutex
	cards       map[string]observedCard
	done        map[string]bool
	fileCalls   int
	fileReady   chan deliveredFile
	fileRelease chan struct{}
	releaseOnce sync.Once
}

func newLocalFeishu() *localFeishu {
	return &localFeishu{cards: map[string]observedCard{}, done: map[string]bool{}, fileReady: make(chan deliveredFile, 1), fileRelease: make(chan struct{})}
}
func (f *localFeishu) releaseFile() { f.releaseOnce.Do(func() { close(f.fileRelease) }) }
func cloneCard(card core.Args) core.Args {
	data, _ := json.Marshal(card)
	var result core.Args
	_ = json.Unmarshal(data, &result)
	return result
}
func (f *localFeishu) Send(ctx context.Context, _ core.Room, _, _, _ string) (string, error) {
	return "notice-" + core.NewID(), ctx.Err()
}
func (f *localFeishu) Card(ctx context.Context, room core.Room, card core.Args, replyTo, _ string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	id := "card-" + core.NewID()
	f.mu.Lock()
	f.cards[id] = observedCard{id: id, replyTo: replyTo, room: room, card: cloneCard(card)}
	f.mu.Unlock()
	return id, nil
}
func (f *localFeishu) Update(ctx context.Context, id string, card core.Args) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.cards[id]
	if !ok {
		return errors.New("update targeted an unobserved card")
	}
	old.card = cloneCard(card)
	f.cards[id] = old
	return nil
}
func (*localFeishu) Download(context.Context, string, core.Resource, string, int64) error {
	return errors.New("fixture never downloads remote resources")
}
func (*localFeishu) Fetch(context.Context, string, core.Room) (*core.Message, error) {
	return nil, nil
}
func (*localFeishu) History(context.Context, core.Room, core.HistoryQuery) (core.HistoryPage, error) {
	return core.HistoryPage{Messages: []core.Message{}}, nil
}
func (f *localFeishu) Receipt(ctx context.Context, id, state, _ string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	if state == "done" {
		f.done[id] = true
	}
	f.mu.Unlock()
	return nil
}
func (f *localFeishu) File(ctx context.Context, _ core.Room, path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.fileCalls++
	f.mu.Unlock()
	select {
	case f.fileReady <- deliveredFile{name: name, text: string(data)}:
	default:
		return "", errors.New("unexpected duplicate export delivery")
	}
	select {
	case <-f.fileRelease:
		return "file-" + core.NewID(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (f *localFeishu) isDone(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done[id]
}
func (f *localFeishu) withButton(label string) (observedCard, core.Args, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, card := range f.cards {
		value := buttonValue(card.card, label)
		if value != nil {
			copy := card
			copy.card = cloneCard(card.card)
			return copy, cloneCard(value), true
		}
	}
	return observedCard{}, nil, false
}
func buttonValue(card core.Args, label string) core.Args {
	var found core.Args
	var walk func(any) bool
	walk = func(value any) bool {
		switch item := value.(type) {
		case map[string]any:
			if item["tag"] == "button" {
				text, _ := item["text"].(map[string]any)
				if text["content"] == label {
					if value, _ := item["value"].(map[string]any); value != nil {
						found = core.Args(value)
						return true
					}
					// Card JSON 2.0 declares click callbacks under behaviors;
					// newer cards no longer carry a top-level value.
					if behaviors, ok := item["behaviors"].([]any); ok {
						for _, raw := range behaviors {
							behavior, _ := raw.(map[string]any)
							if value, _ := behavior["value"].(map[string]any); value != nil {
								found = core.Args(value)
								return true
							}
						}
					}
				}
			}
			for _, child := range item {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range item {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	if walk(card) {
		return found
	}
	return nil
}
func await(t *testing.T, ctx context.Context, description string, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

// This composes the actual gateway, durable SQLite state and native child. Only
// the model endpoint and Feishu network are substitutes. No callback bypasses
// gateway approval/answer handling, and no remote provider is contacted.
func TestRealNativeGatewayApprovalDeliveryAndRestart(t *testing.T) {
	executable := os.Getenv("CLAUDE_NATIVE_PATH")
	if executable == "" {
		t.Skip("set CLAUDE_NATIVE_PATH for the real gateway/native integration contract")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	root := t.TempDir()
	work := filepath.Join(root, "project")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(work, "approved-file.txt")
	completeAnswer := strings.Repeat("COMPLETE_LOCAL_REPLY ", 1400) + "END_OF_COMPLETE_REPLY"
	t.Setenv("FEISHU_INTEGRATION_MODEL_TOKEN", "integration-fixture-token")
	var modelCalls atomic.Int32
	var resumedWithHistory atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") || r.Header.Get("Authorization") != "Bearer integration-fixture-token" {
			t.Errorf("native model request escaped local contract: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
		var body core.Args
		if err != nil || len(data) > 4<<20 || json.Unmarshal(data, &body) != nil || body["model"] != "fixture-model" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":6000}`)
			return
		}
		n := modelCalls.Add(1)
		var content []core.Args
		switch n {
		case 1:
			content = []core.Args{{"type": "tool_use", "id": "write_approved_file", "name": "Write", "input": core.Args{"file_path": target, "content": "gateway-native-approved"}}}
		case 2:
			content = []core.Args{{"type": "tool_use", "id": "ask_real_question", "name": "AskUserQuestion", "input": core.Args{"questions": []core.Args{{"question": "Which delivery option?", "header": "Delivery", "options": []core.Args{{"label": "First", "description": "Use the first fixture option"}, {"label": "Second", "description": "Use the second fixture option"}}, "multiSelect": false}}}}}
		case 3:
			if !strings.Contains(string(data), "First") || !strings.Contains(string(data), "Which delivery option?") {
				t.Error("gateway answer did not reach the native tool result")
			}
			content = []core.Args{{"type": "text", "text": completeAnswer}}
		default:
			if strings.Contains(string(data), "gateway-native-approved") && strings.Contains(string(data), "END_OF_COMPLETE_REPLY") && strings.Contains(string(data), "Which delivery option?") {
				resumedWithHistory.Add(1)
			}
			content = []core.Args{{"type": "text", "text": "RESUMED_LOCAL_REPLY"}}
		}
		writeModel(w, body, fmt.Sprintf("integration-model-%d", n), content)
	}))
	t.Cleanup(server.Close)
	c := config.Defaults()
	c.TenantID, c.AppID, c.AppSecretEnv = "fixture-tenant", "fixture-app", "FEISHU_INTEGRATION_UNUSED_SECRET"
	c.DataDir = filepath.Join(root, "state")
	c.Runtime.ClaudeExecutable = executable
	c.Runtime.MaxWorkers, c.Runtime.IdleMS = 1, 60000
	c.Model.BaseURL, c.Model.AllowHTTP = server.URL, true
	c.Model.TokenEnv = "FEISHU_INTEGRATION_MODEL_TOKEN"
	c.Model.Model, c.Model.SmallModel = "fixture-model", "fixture-model"
	c.Projects = []core.Project{{ID: "fixture", CWD: work, NativeConfig: "isolated", PermissionMode: "default", Maintainers: []string{"owner"}}}
	c.Groups = []core.Group{{ChatID: "fixture-chat", ProjectID: "fixture", Members: []string{"member"}}}
	c.Limits.UpdateMS, c.Limits.ApprovalMS = 50, 45000
	tr := newLocalFeishu()
	var g *gateway.Gateway
	var st *store.Store
	var launches atomic.Int32
	var exits atomic.Int32
	var observedMu sync.Mutex
	var readyIDs []string
	var nativeErrors []string
	var activity []core.WorkerEvent
	log := func(string, core.Args) {}
	start := func() {
		var err error
		st, err = store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		factory := native.NewFactory(c, log)
		wrapped := func(ctx context.Context, room core.Room, session core.Session, workspace core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
			launches.Add(1)
			return factory(ctx, room, session, workspace, func(event core.WorkerEvent) {
				observedMu.Lock()
				if event.Type == "ready" {
					readyIDs = append(readyIDs, event.CCID)
				}
				if event.Type == "error" {
					nativeErrors = append(nativeErrors, event.Text)
				}
				if event.Type == "activity" {
					activity = append(activity, event)
				}
				if event.Type == "exit" {
					exits.Add(1)
				}
				observedMu.Unlock()
				emit(event)
			})
		}
		g = gateway.New(c, st, tr, wrapped, log)
		if err = g.Recover(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		tr.releaseFile()
		if g != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 8*time.Second)
			if err := g.Close(closeCtx); err != nil {
				t.Errorf("gateway/native cleanup was not confirmed: %v", err)
			}
			closeCancel()
		}
		if st != nil {
			if err := st.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	start()
	message := func(id, text, sender string) core.Message {
		return core.Message{ID: id, ChatID: "fixture-chat", ChatType: "group", SenderID: sender, Text: text, Mentioned: true, Time: time.Now().UnixMilli()}
	}
	first := message("initial-input", "Run the approved local fixture journey.", "member")
	if err := g.Message(ctx, first); err != nil {
		t.Fatal(err)
	}
	var permission observedCard
	var allow core.Args
	await(t, ctx, "gateway permission card", func() bool {
		var found bool
		permission, allow, found = tr.withButton("批准一次")
		return found
	})
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("native Write ran before gateway approval: %v", err)
	}
	// An ordinary allowed member is still not a maintainer. The real native
	// Write remains blocked until the exact observed card is approved by owner.
	await(t, ctx, "permission card registration", func() bool {
		text, err := g.Action(ctx, core.CardAction{UserID: "member", ChatID: first.ChatID, MessageID: permission.id, Value: allow})
		if err != nil {
			t.Fatal(err)
		}
		return text == "仅维护者可以审批"
	})
	text, err := g.Action(ctx, core.CardAction{UserID: "owner", ChatID: first.ChatID, MessageID: permission.id, Value: allow})
	if err != nil || text != "已批准一次" {
		t.Fatalf("gateway maintainer approval failed: %q %v", text, err)
	}
	var question core.Args
	await(t, ctx, "gateway AskUserQuestion card", func() bool {
		_, value, found := tr.withButton("提交选择")
		question = value
		return found
	})
	written, err := os.ReadFile(target)
	if err != nil || string(written) != "gateway-native-approved" {
		t.Fatalf("approved native Write did not execute: %q %v", written, err)
	}
	questionID, _ := question["request"].(string)
	if questionID == "" {
		t.Fatal("question card lost its request binding")
	}
	if err = g.Message(ctx, message("answer-input", "/answer "+questionID+" First", "member")); err != nil {
		t.Fatal(err)
	}
	var export deliveredFile
	select {
	case export = <-tr.fileReady:
	case <-ctx.Done():
		t.Fatal("complete reply never reached file delivery")
	}
	if export.name != "reply.md" || export.text != completeAnswer {
		t.Fatalf("reply export lost content: name=%q bytes=%d, want %d", export.name, len(export.text), len(completeAnswer))
	}
	if tr.isDone(first.ID) {
		t.Fatal("Done was emitted before the full reply file was acknowledged")
	}
	var outputState, fileState string
	if err = st.DB.QueryRow("SELECT state,file_state FROM outbox WHERE reply_to=? AND kind='reply'", first.ID).Scan(&outputState, &fileState); err != nil || outputState != "sending" || fileState != "sending" {
		t.Fatalf("pending delivery was not durable: %q %q %v", outputState, fileState, err)
	}
	tr.releaseFile()
	await(t, ctx, "Done after complete delivery", func() bool { return tr.isDone(first.ID) })
	if err = st.DB.QueryRow("SELECT state,file_state FROM outbox WHERE reply_to=? AND kind='reply'", first.ID).Scan(&outputState, &fileState); err != nil || outputState != "done" || fileState != "done" {
		t.Fatalf("acknowledged final output did not commit: %q %q %v", outputState, fileState, err)
	}
	tr.mu.Lock()
	fileCalls := tr.fileCalls
	replyCards := 0
	for _, card := range tr.cards {
		if card.replyTo == first.ID {
			replyCards++
		}
	}
	tr.mu.Unlock()
	if fileCalls != 1 || replyCards != 1 {
		t.Fatalf("reply identity/delivery duplicated: cards=%d files=%d", replyCards, fileCalls)
	}
	room, ok := gateway.Route(c, first)
	if !ok {
		t.Fatal("fixture room was not routed")
	}
	original, err := st.Session(room.Key)
	if err != nil || original.CCID == "" {
		t.Fatalf("native session identity was not saved: %+v %v", original, err)
	}
	// A successful result alone is not enough to reclaim a worker: the real
	// native activity protocol must also confirm no background work or pins.
	// Advancing only Sweep's clock avoids a minute-long test or changing native
	// protocol state from the fixture itself.
	idleCtx, idleCancel := context.WithTimeout(ctx, 5*time.Second)
	for exits.Load() == 0 && idleCtx.Err() == nil {
		g.Sweep(idleCtx, time.Now().Add(2*time.Minute).UnixMilli())
		if exits.Load() != 0 {
			break
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-timer.C:
		case <-idleCtx.Done():
			timer.Stop()
		}
	}
	idleCancel()
	if exits.Load() != 1 {
		observedMu.Lock()
		states := append([]core.WorkerEvent(nil), activity...)
		observedMu.Unlock()
		t.Fatalf("idle Sweep did not confirm native worker reclamation; activity=%+v", states)
	}
	woken := message("idle-wake-input", "Continue after confirmed idle reclamation.", "member")
	if err = g.Message(ctx, woken); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, "reply after idle reclamation", func() bool { return tr.isDone(woken.ID) })
	warm, err := st.Session(room.Key)
	if err != nil || warm.ID != original.ID || warm.CCID != original.CCID || launches.Load() != 2 || resumedWithHistory.Load() != 1 {
		t.Fatalf("idle reclamation lost the native conversation: %+v launches=%d transcript=%d err=%v", warm, launches.Load(), resumedWithHistory.Load(), err)
	}
	if err = g.Message(ctx, message("stop-input", "/stop", "owner")); err != nil {
		t.Fatal(err)
	}
	stopped, err := st.Session(room.Key)
	if err != nil || stopped.ID != original.ID || stopped.CCID != original.CCID {
		t.Fatalf("stop changed the durable native session: %+v %v", stopped, err)
	}
	if err = g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	g = nil
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st = nil
	start()
	resumed := message("resumed-input", "Continue the existing local conversation.", "member")
	if err = g.Message(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, "resumed native reply delivery", func() bool { return tr.isDone(resumed.ID) })
	after, err := st.Session(room.Key)
	if err != nil || after.ID != original.ID || after.CCID != original.CCID || launches.Load() != 3 || resumedWithHistory.Load() != 2 {
		t.Fatalf("restart lost the native conversation: sameSession=%t sameNative=%t launches=%d transcripts=%d err=%v", after.ID == original.ID, after.CCID == original.CCID, launches.Load(), resumedWithHistory.Load(), err)
	}
	observedMu.Lock()
	ids := append([]string(nil), readyIDs...)
	failures := append([]string(nil), nativeErrors...)
	observedMu.Unlock()
	if len(ids) < 3 || len(failures) != 0 {
		t.Fatalf("native lifecycle evidence incomplete: ready=%v errors=%v", ids, failures)
	}
	for _, id := range ids {
		if id != original.CCID {
			t.Fatalf("native process resumed a different ID: %q, expected %q", id, original.CCID)
		}
	}
	if modelCalls.Load() != 5 {
		t.Fatalf("unexpected model retries or replayed tools: %d calls", modelCalls.Load())
	}
	t.Log("real CLI + Gateway + SQLite: maintainer-approved Write, /answer, full-file-before-Done, one reply card, idle reclamation, /stop and durable same-ID restart passed against local fixtures")
}

func writeModel(w http.ResponseWriter, request core.Args, id string, content []core.Args) {
	stop := "end_turn"
	for _, block := range content {
		if block["type"] == "tool_use" {
			stop = "tool_use"
		}
	}
	message := core.Args{"id": id, "type": "message", "role": "assistant", "model": "fixture-model", "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": core.Args{"input_tokens": 6000, "output_tokens": 200, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}
	if streaming, _ := request["stream"].(bool); !streaming {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(kind string, data core.Args) {
		data["type"] = kind
		encoded, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, encoded)
	}
	message["content"], message["stop_reason"] = []core.Args{}, nil
	message["usage"] = core.Args{"input_tokens": 6000, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	emit("message_start", core.Args{"message": message})
	for index, block := range content {
		var initial, delta core.Args
		if block["type"] == "tool_use" {
			input, _ := json.Marshal(block["input"])
			initial = core.Args{"type": "tool_use", "id": block["id"], "name": block["name"], "input": core.Args{}}
			delta = core.Args{"type": "input_json_delta", "partial_json": string(input)}
		} else {
			initial = core.Args{"type": "text", "text": ""}
			delta = core.Args{"type": "text_delta", "text": block["text"]}
		}
		emit("content_block_start", core.Args{"index": index, "content_block": initial})
		emit("content_block_delta", core.Args{"index": index, "delta": delta})
		emit("content_block_stop", core.Args{"index": index})
	}
	emit("message_delta", core.Args{"delta": core.Args{"stop_reason": stop, "stop_sequence": nil}, "usage": core.Args{"output_tokens": 200}})
	emit("message_stop", core.Args{})
}
