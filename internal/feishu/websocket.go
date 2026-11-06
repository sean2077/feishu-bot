package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/sean2077/feishu-bot/internal/core"
)

// We use the official v3.5.3 wire Frame, endpoint and response models, but own
// the connection lifecycle. That release's ws.Client.Start ends in select{},
// and its ping/reconnect loops do not observe context cancellation. Wrapping
// Start in another goroutine would leak a client on every shutdown/reconnect.
// This driver is an independent implementation of that documented protocol;
// it has no SDK private-field access or copied upstream implementation.

func (c *Client) Start(parent context.Context, onMessage func(context.Context, core.Message) error, onAction func(context.Context, core.CardAction) (core.Args, error)) error {
	c.lifeMu.Lock()
	if c.closed || c.started {
		c.lifeMu.Unlock()
		return errors.New("feishu: transport already started or closed")
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.started = true
	c.onMessage = onMessage
	c.onAction = onAction
	c.lifeMu.Unlock()
	defer func() { cancel(); c.lifeMu.Lock(); c.started = false; c.cancel = nil; c.lifeMu.Unlock() }()
	if c.botOpenID() == "" {
		var bot struct {
			ID string `json:"open_id"`
		}
		if err := c.api(ctx, http.MethodGet, "/bot/v3/info", nil, nil, &bot); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !validID(bot.ID) {
			return errors.New("feishu: bot identity is unavailable")
		}
		c.setBotOpenID(bot.ID)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		endpoint, err := c.endpoint(ctx)
		if err == nil {
			err = c.connection(ctx, endpoint)
			backoff = time.Second
		}
		if ctx.Err() != nil {
			return nil
		}
		var permanent *endpointError
		if errors.As(err, &permanent) && permanent.permanent {
			return permanent
		}
		c.emit("feishu_ws_reconnect", formatSafeError(err))
		wait := backoff
		if endpoint != nil && endpoint.ClientConfig != nil && endpoint.ClientConfig.ReconnectInterval > 0 {
			wait = time.Duration(endpoint.ClientConfig.ReconnectInterval) * time.Second
			if wait > 5*time.Minute {
				wait = 5 * time.Minute
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < time.Minute {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
	return nil
}
func (c *Client) Close() error {
	c.lifeMu.Lock()
	c.closed = true
	cancel := c.cancel
	conn := c.conn
	c.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	c.budget.close()
	c.http.CloseIdleConnections()
	return nil
}

type endpointError struct {
	code      int
	permanent bool
}

func (e *endpointError) Error() string {
	return "feishu: websocket endpoint rejected (code " + strconv.Itoa(e.code) + ")"
}
func (c *Client) endpoint(ctx context.Context) (*larkws.Endpoint, error) {
	payload, _ := json.Marshal(map[string]string{"AppID": c.config.AppID, "AppSecret": c.secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+larkws.GenEndpointUri, bytes.NewReader(payload))
	if err != nil {
		return nil, safeFailure("websocket endpoint", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("locale", "zh")
	response, err := c.doHTTP(req, c.http)
	if err != nil {
		return nil, safeFailure("websocket endpoint", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusTooManyRequests {
			c.budget.rateLimited(req, responseRetryAfter(response.Header, time.Now()))
		}
		return nil, &endpointError{code: response.StatusCode, permanent: response.StatusCode == 401 || response.StatusCode == 403}
	}
	var result larkws.EndpointResp
	if readJSON(response.Body, &result, 64<<10) != nil {
		return nil, errors.New("feishu: invalid websocket endpoint response")
	}
	if result.Code != 0 {
		if result.Code == 99991400 {
			c.budget.rateLimited(req, responseRetryAfter(response.Header, time.Now()))
		}
		return nil, &endpointError{code: result.Code, permanent: result.Code != larkws.SystemBusy && result.Code != larkws.InternalError && result.Code != 99991400}
	}
	if result.Data == nil || len(result.Data.Url) > 16<<10 {
		return nil, errors.New("feishu: missing websocket endpoint")
	}
	parsed, err := url.Parse(result.Data.Url)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "wss" && !(strings.HasPrefix(c.base, "http://") && parsed.Scheme == "ws")) {
		return nil, errors.New("feishu: invalid websocket endpoint")
	}
	return result.Data, nil
}

type connectionJob struct {
	frame larkws.Frame
	event pendingEvent
	received time.Time
}

func (c *Client) connection(parent context.Context, endpoint *larkws.Endpoint) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	conn, response, err := c.dialer.DialContext(ctx, endpoint.Url, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		if response != nil {
			code, _ := strconv.Atoi(response.Header.Get(larkws.HeaderHandshakeStatus))
			authCode, _ := strconv.Atoi(response.Header.Get(larkws.HeaderHandshakeAuthErrCode))
			if response.StatusCode == http.StatusForbidden || code == larkws.Forbidden || authCode == larkws.ExceedConnLimit {
				return &endpointError{code: code, permanent: true}
			}
		}
		return safeFailure("websocket handshake", err)
	}
	defer conn.Close()
	conn.SetReadLimit(2 << 20)
	c.lifeMu.Lock()
	if c.closed {
		c.lifeMu.Unlock()
		return nil
	}
	c.conn = conn
	c.lifeMu.Unlock()
	defer func() {
		c.lifeMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.lifeMu.Unlock()
	}()
	var writeMu sync.Mutex
	write := func(frame larkws.Frame) error {
		data, err := frame.Marshal()
		if err != nil {
			return errors.New("feishu: invalid websocket frame")
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, data)
	}
	ack := func(frame larkws.Frame, reply any, handleErr error, received time.Time) {
		result := larkws.NewResponseByCode(http.StatusOK)
		if handleErr != nil {
			result = larkws.NewResponseByCode(http.StatusInternalServerError)
		} else if reply != nil {
			result.Data, _ = json.Marshal(reply)
		}
		frame.Payload, _ = json.Marshal(result)
		failed := write(frame) != nil
		c.wsStats.acknowledge(received, failed)
		if failed {
			_ = conn.Close()
		}
	}
	// Per-chat hashing preserves arrival order with bounded storage. Actions get
	// their own queue, so a queued chat cannot hold up a permission callback.
	var jobs [4]chan connectionJob
	actionJobs := make(chan connectionJob, 8)
	var workers sync.WaitGroup
	run := func(queue <-chan connectionJob) {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-queue:
				c.wsStats.queued(job.received)
				started := time.Now()
				reply, err := c.runEvent(ctx, job.event)
				job.frame.Headers = append(job.frame.Headers, larkws.Header{Key: larkws.HeaderBizRt, Value: strconv.FormatInt(time.Since(started).Milliseconds(), 10)})
				ack(job.frame, reply, err, job.received)
			}
		}
	}
	for i := range jobs {
		jobs[i] = make(chan connectionJob, 8)
		workers.Add(1)
		go run(jobs[i])
	}
	workers.Add(1)
	go run(actionJobs)
	c.wsStats.connect([5]<-chan connectionJob{jobs[0], jobs[1], jobs[2], jobs[3], actionJobs})
	defer func() { cancel(); _ = conn.Close(); workers.Wait(); c.wsStats.disconnect() }()
	var lastInbound atomic.Int64
	lastInbound.Store(time.Now().UnixNano())
	var pingNanos atomic.Int64
	pingNanos.Store(int64(pingPeriod(endpoint.ClientConfig)))
	parsed, _ := url.Parse(endpoint.Url)
	service, _ := strconv.ParseInt(parsed.Query().Get(larkws.ServiceID), 10, 32)
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		next := time.Now()
		nextDiagnostics := next.Add(30 * time.Second)
		var pending time.Time
		for {
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case now := <-ticker.C:
				if !now.Before(nextDiagnostics) {
					c.emit("feishu_transport_metrics", c.Diagnostics())
					nextDiagnostics = now.Add(30 * time.Second)
				}
				if !pending.IsZero() && lastInbound.Load() >= pending.UnixNano() {
					pending = time.Time{}
				}
				if !pending.IsZero() && now.Sub(pending) > 30*time.Second {
					c.wsStats.watchdog()
					c.emit("feishu_ws_watchdog", nil)
					_ = conn.Close()
					return
				}
				if !now.Before(next) {
					if write(*larkws.NewPingFrame(int32(service))) != nil {
						_ = conn.Close()
						return
					}
					if pending.IsZero() {
						pending = now
					}
					next = now.Add(time.Duration(pingNanos.Load()))
				}
			}
		}
	}()
	defer func() { cancel(); <-pingDone }()
	assembler := newAssembler()
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return safeFailure("websocket read", err)
		}
		received := time.Now()
		lastInbound.Store(received.UnixNano())
		c.wsStats.inbound(received)
		var frame larkws.Frame
		if frame.Unmarshal(payload) != nil {
			_ = conn.Close()
			return errors.New("feishu: malformed websocket frame")
		}
		if len(frame.Headers) > 64 {
			return errors.New("feishu: websocket header limit")
		}
		headerBytes := 0
		for _, header := range frame.Headers {
			headerBytes += len(header.Key) + len(header.Value)
		}
		if headerBytes > 16<<10 {
			return errors.New("feishu: websocket header byte limit")
		}
		headers := larkws.Headers(frame.Headers)
		if larkws.FrameType(frame.Method) == larkws.FrameTypeControl {
			if headers.GetString(larkws.HeaderType) == string(larkws.MessageTypePong) && len(frame.Payload) > 0 {
				var conf larkws.ClientConfig
				if json.Unmarshal(frame.Payload, &conf) == nil {
					pingNanos.Store(int64(pingPeriod(&conf)))
				}
			}
			continue
		}
		if larkws.FrameType(frame.Method) != larkws.FrameTypeData {
			continue
		}
		kind := headers.GetString(larkws.HeaderType)
		if kind != string(larkws.MessageTypeEvent) && kind != string(larkws.MessageTypeCard) {
			continue
		}
		combined, complete, err := assembler.add(frame, time.Now())
		if err != nil {
			ack(frame, nil, err, received)
			continue
		}
		if !complete {
			continue
		}
		event, err := c.prepareEvent(combined)
		// Drop raw event bytes before queuing; frames retain only acknowledgement IDs.
		frame.Payload = nil
		if err != nil {
			ack(frame, nil, err, received)
			continue
		}
		if event.message == nil && event.action == nil {
			ack(frame, nil, nil, received)
			continue
		}
		queue := actionJobs
		lane := 4
		if event.message != nil {
			hash := fnv.New32a()
			_, _ = hash.Write([]byte(event.message.ChatID))
			lane = int(hash.Sum32() % uint32(len(jobs)))
			queue = jobs[lane]
		}
		select {
		case queue <- connectionJob{frame: frame, event: event, received: received}:
			c.wsStats.admitted(lane)
		default:
			c.wsStats.refused(lane)
			ack(frame, nil, errors.New("feishu: event queue is full"), received)
		}
	}
}
func pingPeriod(config *larkws.ClientConfig) time.Duration {
	if config != nil && config.PingInterval > 0 {
		n := config.PingInterval
		if n > 600 {
			n = 600
		}
		return time.Duration(n) * time.Second
	}
	return 2 * time.Minute
}

type frameParts struct {
	at              time.Time
	chunks          [][]byte
	bytes, received int
}
type frameAssembler struct {
	pending map[string]*frameParts
	bytes   int
}

func newAssembler() *frameAssembler { return &frameAssembler{pending: map[string]*frameParts{}} }
func (a *frameAssembler) add(frame larkws.Frame, now time.Time) ([]byte, bool, error) {
	for key, p := range a.pending {
		if now.Sub(p.at) > 30*time.Second {
			a.bytes -= p.bytes
			delete(a.pending, key)
		}
	}
	headers := larkws.Headers(frame.Headers)
	sum := headers.GetInt(larkws.HeaderSum)
	seq := headers.GetInt(larkws.HeaderSeq)
	if sum <= 1 {
		if len(frame.Payload) > 2<<20 {
			return nil, false, errors.New("feishu: event payload exceeds limit")
		}
		return frame.Payload, true, nil
	}
	id := headers.GetString(larkws.HeaderMessageID)
	if !validID(id) || sum > 128 || seq < 0 || seq >= sum {
		return nil, false, errors.New("feishu: invalid multipart frame")
	}
	parts := a.pending[id]
	if parts == nil {
		if len(a.pending) >= 64 {
			return nil, false, errors.New("feishu: multipart frame limit")
		}
		parts = &frameParts{at: now, chunks: make([][]byte, sum)}
		a.pending[id] = parts
	}
	if len(parts.chunks) != sum {
		return nil, false, errors.New("feishu: inconsistent multipart frame")
	}
	if parts.chunks[seq] != nil {
		return nil, false, nil
	}
	if parts.bytes+len(frame.Payload) > 2<<20 || a.bytes+len(frame.Payload) > 8<<20 {
		a.bytes -= parts.bytes
		delete(a.pending, id)
		return nil, false, errors.New("feishu: multipart byte limit")
	}
	parts.chunks[seq] = append([]byte{}, frame.Payload...)
	parts.bytes += len(frame.Payload)
	a.bytes += len(frame.Payload)
	parts.received++
	if parts.received != sum {
		return nil, false, nil
	}
	result := make([]byte, 0, parts.bytes)
	for _, chunk := range parts.chunks {
		result = append(result, chunk...)
	}
	a.bytes -= parts.bytes
	delete(a.pending, id)
	return result, true, nil
}
