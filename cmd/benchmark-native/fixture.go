package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

type transportStats struct {
	Cards            int64 `json:"cards"`
	Updates          int64 `json:"updates"`
	CardJSONBytes    int64 `json:"cardJsonBytes"`
	MaxCardJSONBytes int64 `json:"maxCardJsonBytes"`
	Downloads        int64 `json:"downloads"`
	DownloadBytes    int64 `json:"downloadBytes"`
	Files            int64 `json:"files"`
	FileBytes        int64 `json:"fileBytes"`
}
type transport struct {
	onReceipt                                  func(string, string)
	delay                                      time.Duration
	cards, updates, cardBytes, maxCardBytes    atomic.Int64
	downloads, downloadBytes, files, fileBytes atomic.Int64
}

func (t *transport) snapshot() transportStats {
	return transportStats{Cards: t.cards.Load(), Updates: t.updates.Load(), CardJSONBytes: t.cardBytes.Load(), MaxCardJSONBytes: t.maxCardBytes.Load(), Downloads: t.downloads.Load(), DownloadBytes: t.downloadBytes.Load(), Files: t.files.Load(), FileBytes: t.fileBytes.Load()}
}
func (*transport) Send(context.Context, core.Room, string, string, string) (string, error) {
	return core.NewID(), nil
}
func (t *transport) card(ctx context.Context, body core.Args) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	t.cardBytes.Add(int64(len(data)))
	for old := t.maxCardBytes.Load(); int64(len(data)) > old; old = t.maxCardBytes.Load() {
		if t.maxCardBytes.CompareAndSwap(old, int64(len(data))) {
			break
		}
	}
	if t.delay > 0 {
		timer := time.NewTimer(t.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
func (t *transport) Card(ctx context.Context, _ core.Room, body core.Args, _, _ string) (string, error) {
	t.cards.Add(1)
	if err := t.card(ctx, body); err != nil {
		return "", err
	}
	return core.NewID(), nil
}
func (t *transport) Update(ctx context.Context, _ string, body core.Args) error {
	t.updates.Add(1)
	return t.card(ctx, body)
}
func (t *transport) Download(ctx context.Context, _ string, resource core.Resource, path string, limit int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if resource.Key != "fixture-attachment" || resource.Kind != "file" || limit < 1<<20 {
		return errors.New("unexpected fixture attachment")
	}
	data := strings.Repeat("fixture\n", (1<<20)/8)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		return err
	}
	t.downloads.Add(1)
	t.downloadBytes.Add(int64(len(data)))
	return nil
}
func (*transport) Fetch(context.Context, string, core.Room) (*core.Message, error) { return nil, nil }
func (*transport) History(context.Context, core.Room, core.HistoryQuery) (core.HistoryPage, error) {
	return core.HistoryPage{Messages: []core.Message{}}, nil
}
func (t *transport) Receipt(_ context.Context, id, state, _ string) error {
	if t.onReceipt != nil {
		t.onReceipt(id, state)
	}
	return nil
}
func (t *transport) File(ctx context.Context, _ core.Room, path, _ string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", errors.New("empty fixture output")
	}
	t.files.Add(1)
	t.fileBytes.Add(int64(len(data)))
	return core.NewID(), nil
}

// Sizes are UTF-8 bytes, not runes. The final ASCII bytes preserve exact sizes.
func benchmarkReply(size int) string {
	if size <= 0 {
		return fixtureReply
	}
	return strings.Repeat("中", size/3) + strings.Repeat("A", size%3)
}
func modelFixture() (*httptest.Server, *atomic.Int64) { return modelFixtureReply(fixtureReply) }
func modelFixtureReply(reply string) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") {
			http.Error(w, "fixture endpoint only", 404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fixtureToken {
			http.Error(w, "fixture authorization required", http.StatusUnauthorized)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, (8<<20)+1))
		if err != nil || len(data) > 8<<20 {
			http.Error(w, "fixture input limit", 400)
			return
		}
		var body core.Args
		if json.Unmarshal(data, &body) != nil {
			http.Error(w, "fixture JSON", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if body["model"] != "fixture-model" {
			http.Error(w, "fixture model required", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(w, `{"input_tokens":500}`)
			return
		}
		n := calls.Add(1)
		usage := core.Args{"input_tokens": 500, "output_tokens": 30, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
		message := core.Args{"id": fmt.Sprintf("msg_fixture_%d", n), "type": "message", "role": "assistant", "model": "fixture-model", "content": []core.Args{{"type": "text", "text": reply}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": usage}
		if stream, _ := body["stream"].(bool); !stream {
			_ = json.NewEncoder(w).Encode(message)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, a core.Args) {
			a["type"] = kind
			data, _ := json.Marshal(a)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		message["content"] = []core.Args{}
		message["stop_reason"] = nil
		message["usage"] = core.Args{"input_tokens": 500, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
		event("message_start", core.Args{"message": message})
		event("content_block_start", core.Args{"index": 0, "content_block": core.Args{"type": "text", "text": ""}})
		for rest := reply; len(rest) > 0; {
			n := min(len(rest), 768)
			for n < len(rest) && !utf8.RuneStart(rest[n]) {
				n--
			}
			event("content_block_delta", core.Args{"index": 0, "delta": core.Args{"type": "text_delta", "text": rest[:n]}})
			rest = rest[n:]
		}
		event("content_block_stop", core.Args{"index": 0})
		event("message_delta", core.Args{"delta": core.Args{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": core.Args{"output_tokens": 30}})
		event("message_stop", core.Args{})
	}))
	return server, &calls
}
