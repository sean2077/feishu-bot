package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/sean2077/feishu-bot/internal/core"
)

func wireEvent(id string, payload []byte) larkws.Frame {
	return larkws.Frame{SeqID: 1, LogID: 2, Method: int32(larkws.FrameTypeData), Service: 3, Headers: []larkws.Header{{Key: larkws.HeaderType, Value: string(larkws.MessageTypeEvent)}, {Key: larkws.HeaderMessageID, Value: id}, {Key: larkws.HeaderSum, Value: "1"}, {Key: larkws.HeaderSeq, Value: "0"}}, Payload: payload}
}
func writeFrame(t *testing.T, conn *websocket.Conn, frame larkws.Frame) bool {
	t.Helper()
	b, err := frame.Marshal()
	if err != nil {
		t.Error(err)
		return false
	}
	if err = conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
		t.Error("fixture frame write failed")
		return false
	}
	return true
}
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out: " + what)
	}
}
func TestLongConnectionSeparatesCardCallbacksAndClosesAllLoops(t *testing.T) {
	var origin string
	cardAck := make(chan struct{})
	serverClosed := make(chan struct{})
	messageStarted := make(chan struct{})
	var started sync.Once
	upgrader := websocket.Upgrader{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback/ws/endpoint" {
			jsonResponse(w, 200, map[string]any{"code": 0, "data": map[string]any{"URL": strings.Replace(origin, "http://", "ws://", 1) + "/ws?service_id=3&device_id=fixture", "ClientConfig": map[string]int{"PingInterval": 1, "ReconnectInterval": 1}}})
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error("fixture upgrade failed")
			return
		}
		defer conn.Close()
		defer close(serverClosed)
		if !writeFrame(t, conn, wireEvent("wire_message", rawEvent("oc_allowed", "ou_member", "text", `{"text":"@_user_1 hello"}`, true))) {
			return
		}
		action := []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger"},"event":{"operator":{"open_id":"ou_owner"},"context":{"open_chat_id":"oc_allowed","open_message_id":"om_approval"},"action":{"value":{"requestId":"fixture-request","choice":"allow"}}}}`)
		if !writeFrame(t, conn, wireEvent("wire_action", action)) {
			return
		}
		for {
			_, b, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame larkws.Frame
			if frame.Unmarshal(b) != nil {
				t.Error("invalid client frame")
				return
			}
			headers := larkws.Headers(frame.Headers)
			if headers.GetString(larkws.HeaderMessageID) == "wire_action" {
				var response larkws.Response
				if json.Unmarshal(frame.Payload, &response) != nil || response.StatusCode != 200 || !strings.Contains(string(response.Data), "确认") {
					t.Error("invalid callback acknowledgement")
				}
				close(cardAck)
			}
		}
	}
	c, server := fixtureClient(t, nil, handler)
	origin = server.URL
	done := make(chan error, 1)
	go func() {
		done <- c.Start(context.Background(), func(ctx context.Context, _ core.Message) error {
			started.Do(func() { close(messageStarted) })
			<-ctx.Done()
			return ctx.Err()
		}, func(context.Context, core.CardAction) (core.Args, error) {
			return core.Args{"toast": core.Args{"type": "info", "content": "已确认"}}, nil
		})
	}()
	await(t, messageStarted, "message callback")
	await(t, cardAck, "independent card callback")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit after Close")
	}
	await(t, serverClosed, "server connection teardown")
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.conn != nil || c.started || c.cancel != nil {
		t.Fatal("connection lifetime retained after shutdown")
	}
}
func TestLongConnectionReconnectsAndHonorsContext(t *testing.T) {
	var origin string
	var connections atomic.Int32
	ready := make(chan struct{})
	upgrader := websocket.Upgrader{}
	c, server := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback/ws/endpoint" {
			jsonResponse(w, 200, map[string]any{"code": 0, "data": map[string]any{"URL": strings.Replace(origin, "http://", "ws://", 1) + "/ws?service_id=3", "ClientConfig": map[string]int{"ReconnectInterval": 1, "PingInterval": 1}}})
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if connections.Add(1) == 1 {
			return
		}
		close(ready)
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	origin = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx, ignoreMessage, nil) }()
	await(t, ready, "automatic reconnect")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not stop reconnect client")
	}
	if connections.Load() != 2 {
		t.Fatalf("unexpected connections: %d", connections.Load())
	}
}
func TestMultipartFramesBoundMemoryAndDiscardExpiredPieces(t *testing.T) {
	assembler := newAssembler()
	now := time.Now()
	frame := wireEvent("fragmented", []byte("world"))
	frame.Headers[2].Value = "2"
	frame.Headers[3].Value = "1"
	if _, complete, err := assembler.add(frame, now); err != nil || complete {
		t.Fatal("out of order first fragment failed")
	}
	frame.Payload = []byte("hello ")
	frame.Headers[3].Value = "0"
	result, complete, err := assembler.add(frame, now)
	if err != nil || !complete || string(result) != "hello world" || assembler.bytes != 0 || len(assembler.pending) != 0 {
		t.Fatal("fragment reassembly failed")
	}
	frame.Headers[0].Value = "event"
	frame.Headers[1].Value = "expired"
	frame.Headers[3].Value = "0"
	_, _, _ = assembler.add(frame, now)
	_, _, _ = assembler.add(wireEvent("other", []byte("{}")), now.Add(31*time.Second))
	if assembler.bytes != 0 || len(assembler.pending) != 0 {
		t.Fatal("expired fragment retained")
	}
	frame.Headers[2].Value = "129"
	if _, _, err = assembler.add(frame, now); err == nil {
		t.Fatal("unbounded fragment count accepted")
	}
	frame.Headers[2].Value = "2"
	frame.Payload = make([]byte, 2<<20)
	_, _, err = assembler.add(frame, now)
	if err != nil {
		t.Fatal(err)
	}
	frame.Headers[3].Value = "1"
	frame.Payload = []byte("overflow")
	if _, _, err = assembler.add(frame, now); err == nil || assembler.bytes != 0 {
		t.Fatal("oversize reassembly retained")
	}
}
