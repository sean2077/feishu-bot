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

// The peer keeps reading client frames even when it stops answering. Closing the
// server socket would only exercise ordinary read-error recovery, not a live TCP
// connection whose inbound event and heartbeat path has gone silent.
type recoveryPeer struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	silent  atomic.Bool
	pings   chan time.Time
	pongs   chan time.Time
	acks    chan recoveryAck
	closed  chan struct{}
}

type recoveryAck struct {
	id     string
	status int
}

func (p *recoveryPeer) write(frame larkws.Frame) error {
	data, err := frame.Marshal()
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (p *recoveryPeer) sendMessage(t *testing.T, chat, id, author string) {
	t.Helper()
	payload := rawEvent(chat, author, "text", `{"text":"@_user_1 `+id+`"}`, true)
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["event"].(map[string]any)["message"].(map[string]any)["message_id"] = id
	payload, _ = json.Marshal(envelope)
	if err := p.write(wireEvent(id, payload)); err != nil {
		t.Fatalf("send fixture message: %v", err)
	}
}

func (p *recoveryPeer) awaitAck(t *testing.T, id string) {
	t.Helper()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-p.acks:
			if got.id == id {
				if got.status != http.StatusOK {
					t.Fatalf("acknowledgement for %s returned %d", id, got.status)
				}
				return
			}
		case <-timer.C:
			t.Fatalf("no acknowledgement for %s", id)
		}
	}
}

type recoveryServer struct {
	peers       chan *recoveryPeer
	connections atomic.Int32
	active      atomic.Int32
	mu          sync.Mutex
	all         []*recoveryPeer
}

func newRecoveryClient(t *testing.T) (*Client, *recoveryServer) {
	t.Helper()
	g := &recoveryServer{peers: make(chan *recoveryPeer, 8)}
	var origin string
	upgrader := websocket.Upgrader{}
	client, server := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback/ws/endpoint" {
			jsonResponse(w, 200, core.Args{"code": 0, "data": core.Args{
				"URL":          strings.Replace(origin, "http://", "ws://", 1) + "/ws?service_id=3",
				"ClientConfig": core.Args{"ReconnectInterval": 1, "PingInterval": 1},
			}})
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		peer := &recoveryPeer{conn: conn, pings: make(chan time.Time, 16), pongs: make(chan time.Time, 16), acks: make(chan recoveryAck, 16), closed: make(chan struct{})}
		g.connections.Add(1)
		g.active.Add(1)
		g.mu.Lock()
		g.all = append(g.all, peer)
		g.mu.Unlock()
		defer func() {
			_ = conn.Close()
			g.active.Add(-1)
			close(peer.closed)
		}()
		g.peers <- peer
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame larkws.Frame
			if frame.Unmarshal(data) != nil {
				t.Error("invalid client recovery frame")
				return
			}
			headers := larkws.Headers(frame.Headers)
			if headers.GetString(larkws.HeaderType) == string(larkws.MessageTypePing) {
				select {
				case peer.pings <- time.Now():
				default:
				}
				if peer.silent.Load() {
					continue
				}
				frame.Headers = []larkws.Header{{Key: larkws.HeaderType, Value: string(larkws.MessageTypePong)}}
				frame.Payload = []byte(`{"PingInterval":1}`)
				if err := peer.write(frame); err != nil {
					return
				}
				select {
				case peer.pongs <- time.Now():
				default:
				}
			} else {
				var response larkws.Response
				if json.Unmarshal(frame.Payload, &response) != nil {
					t.Error("invalid recovery event acknowledgement")
					return
				}
				select {
				case peer.acks <- recoveryAck{headers.GetString(larkws.HeaderMessageID), response.StatusCode}:
				default:
				}
			}
		}
	})
	origin = server.URL
	client.config.Groups = append(client.config.Groups, core.Group{ChatID: "oc_other", ProjectID: "fixture", Members: []string{"ou_member"}})
	t.Cleanup(func() {
		g.mu.Lock()
		peers := append([]*recoveryPeer(nil), g.all...)
		g.mu.Unlock()
		for _, peer := range peers {
			_ = peer.conn.Close()
			await(t, peer.closed, "fixture connection shutdown")
		}
	})
	return client, g
}

func (g *recoveryServer) nextPeer(t *testing.T) *recoveryPeer {
	t.Helper()
	select {
	case peer := <-g.peers:
		return peer
	case <-time.After(4 * time.Second):
		t.Fatal("client did not establish the next connection")
		return nil
	}
}

const recoveryInboundTimeout = 500 * time.Millisecond

func startRecoveryClient(t *testing.T, c *Client, onMessage func(context.Context, core.Message) error, onAction func(context.Context, core.CardAction) (core.Args, error)) <-chan struct{} {
	t.Helper()
	stopped := make(chan struct{})
	var result error
	go func() {
		result = c.start(context.Background(), onMessage, onAction, websocketTiming{tick: 10 * time.Millisecond, inboundTimeout: recoveryInboundTimeout})
		close(stopped)
	}()
	t.Cleanup(func() {
		_ = c.Close()
		await(t, stopped, "transport shutdown")
		if result != nil {
			t.Errorf("transport shutdown: %v", result)
		}
	})
	return stopped
}

func awaitRecoveryMessage(t *testing.T, messages <-chan string, want string) {
	t.Helper()
	select {
	case got := <-messages:
		if got != want {
			t.Fatalf("received %s, want %s", got, want)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("message %s was not delivered", want)
	}
}

func TestLongConnectionRecoversSilentInbound(t *testing.T) {
	c, server := newRecoveryClient(t)
	messages := make(chan string, 4)
	startRecoveryClient(t, c, func(_ context.Context, m core.Message) error { messages <- m.ID; return nil }, nil)
	first := server.nextPeer(t)
	first.silent.Store(true)
	first.sendMessage(t, "oc_allowed", "om_before_silence", "ou_member")
	awaitRecoveryMessage(t, messages, "om_before_silence")
	select {
	case <-first.pings:
	case <-time.After(4 * time.Second):
		t.Fatal("silent peer did not keep accepting client pings")
	}
	await(t, first.closed, "watchdog closing the silent socket")
	second := server.nextPeer(t)
	second.sendMessage(t, "oc_allowed", "om_after_silence", "ou_member")
	awaitRecoveryMessage(t, messages, "om_after_silence")
	second.awaitAck(t, "om_after_silence")
	if server.connections.Load() != 2 || server.active.Load() != 1 {
		t.Fatalf("connections = %d, active = %d; want one replacement socket", server.connections.Load(), server.active.Load())
	}
	stats := c.Diagnostics()["websocket"].(core.Args)
	if stats["watchdogs"] != uint64(1) {
		t.Fatalf("silent connection was not recovered by the watchdog: %v", stats)
	}
}

func TestLongConnectionHealthyIdleKeepsSocket(t *testing.T) {
	c, server := newRecoveryClient(t)
	messages := make(chan string, 1)
	startRecoveryClient(t, c, func(_ context.Context, m core.Message) error { messages <- m.ID; return nil }, nil)
	peer := server.nextPeer(t)
	var first, last time.Time
	for i := 0; i < 3; i++ {
		select {
		case last = <-peer.pongs:
			if i == 0 {
				first = last
			}
		case <-time.After(4 * time.Second):
			t.Fatal("healthy idle peer stopped receiving heartbeats")
		}
	}
	if last.Sub(first) <= 2*recoveryInboundTimeout {
		t.Fatal("fixture did not remain idle beyond the watchdog window")
	}
	peer.sendMessage(t, "oc_allowed", "om_after_healthy_idle", "ou_member")
	awaitRecoveryMessage(t, messages, "om_after_healthy_idle")
	peer.awaitAck(t, "om_after_healthy_idle")
	stats := c.Diagnostics()["websocket"].(core.Args)
	if server.connections.Load() != 1 || stats["watchdogs"] != uint64(0) {
		t.Fatalf("healthy heartbeats caused a reconnect: %v", stats)
	}
}

func TestLongConnectionCloseCancelsHandlersAndStops(t *testing.T) {
	c, server := newRecoveryClient(t)
	messageStarted, actionStarted := make(chan struct{}), make(chan struct{})
	messageStopped, actionStopped := make(chan struct{}), make(chan struct{})
	stopped := startRecoveryClient(t, c, func(ctx context.Context, _ core.Message) error {
		close(messageStarted)
		<-ctx.Done()
		close(messageStopped)
		return ctx.Err()
	}, func(ctx context.Context, _ core.CardAction) (core.Args, error) {
		close(actionStarted)
		<-ctx.Done()
		close(actionStopped)
		return nil, ctx.Err()
	})
	peer := server.nextPeer(t)
	peer.sendMessage(t, "oc_allowed", "om_stop_handler", "ou_member")
	action := []byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger"},"event":{"operator":{"open_id":"ou_owner"},"context":{"open_chat_id":"oc_allowed","open_message_id":"om_approval"},"action":{"value":{"requestId":"fixture-request","choice":"allow"}}}}`)
	if err := peer.write(wireEvent("wire_stop_action", action)); err != nil {
		t.Fatal(err)
	}
	await(t, messageStarted, "message handler")
	await(t, actionStarted, "independent action handler")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, stopped, "Start joining all handlers")
	await(t, messageStopped, "message cancellation")
	await(t, actionStopped, "action cancellation")
	await(t, peer.closed, "socket close")
	stats := c.Diagnostics()["websocket"].(core.Args)
	if server.active.Load() != 0 || stats["connected"] != false {
		t.Fatalf("transport kept a connection after shutdown: %v", stats)
	}
	for _, lane := range stats["lanes"].([]core.Args) {
		if lane["depth"] != 0 {
			t.Fatalf("transport retained queued events: %v", lane)
		}
	}
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.conn != nil || c.started || c.cancel != nil {
		t.Fatal("transport lifecycle was not released")
	}
}

func TestLongConnectionReconnectPreservesAdmittedEvents(t *testing.T) {
	c, server := newRecoveryClient(t)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	ordered, independent := make(chan string, 4), make(chan string, 1)
	startRecoveryClient(t, c, func(ctx context.Context, m core.Message) error {
		if m.ID == "om_first" {
			started <- ctx
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if m.ChatID == "oc_other" {
			independent <- m.ID
		} else {
			ordered <- m.ID
		}
		return nil
	}, nil)
	first := server.nextPeer(t)
	first.sendMessage(t, "oc_allowed", "om_first", "ou_member")
	var handlerContext context.Context
	select {
	case handlerContext = <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("first handler did not start")
	}
	first.sendMessage(t, "oc_allowed", "om_queued", "ou_member")
	// This unauthorized frame is acknowledged by the read loop. Its ACK proves
	// the preceding authorized message reached the bounded queue before silence.
	first.sendMessage(t, "oc_allowed", "om_queue_barrier", "ou_stranger")
	first.awaitAck(t, "om_queue_barrier")
	first.silent.Store(true)
	await(t, first.closed, "silent first connection closure")
	second := server.nextPeer(t)
	if err := handlerContext.Err(); err != nil {
		t.Fatalf("reconnect canceled an admitted handler: %v", err)
	}
	// A different chat must remain usable while the old handler is still busy.
	second.sendMessage(t, "oc_other", "om_independent", "ou_member")
	awaitRecoveryMessage(t, independent, "om_independent")
	second.sendMessage(t, "oc_allowed", "om_after_reconnect", "ou_member")
	select {
	case got := <-ordered:
		t.Fatalf("reconnect reordered or duplicated chat input: %s", got)
	default:
	}
	unblock()
	for _, id := range []string{"om_first", "om_queued", "om_after_reconnect"} {
		awaitRecoveryMessage(t, ordered, id)
	}
	// Late ACKs for the old socket must not close or replace the new one.
	second.awaitAck(t, "om_after_reconnect")
	select {
	case id := <-ordered:
		t.Fatalf("reconnect duplicated admitted input: %s", id)
	default:
	}
	if server.connections.Load() != 2 || server.active.Load() != 1 || handlerContext.Err() != nil {
		t.Fatalf("late old-connection completion disturbed recovery: connections=%d active=%d context=%v", server.connections.Load(), server.active.Load(), handlerContext.Err())
	}
}
