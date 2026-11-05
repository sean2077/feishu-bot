// Package feishu provides the bounded Feishu REST and long-connection transport.
package feishu

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sean2077/feishu-bot/internal/core"
)

// Options is also the test seam: tests supply a local HTTP server and dialer,
// never credentials for, or a connection to, the real Feishu service.
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Dialer     *websocket.Dialer
	BotOpenID  string
}

type Client struct {
	config          core.Config
	log             core.Logger
	db              *sql.DB
	base            string
	http            *http.Client
	dialer          *websocket.Dialer
	secret          string
	identityMu      sync.RWMutex
	botID           string
	tokenMu         sync.Mutex
	token           string
	tokenUntil      time.Time
	lifeMu          sync.Mutex
	cancel          context.CancelFunc
	conn            *websocket.Conn
	started, closed bool
	onMessage       func(context.Context, core.Message) error
	onAction        func(context.Context, core.CardAction) (core.Args, error)
	locks           keyedLocks
	originMu        sync.Mutex
	origins         map[string]core.Room
	receiptMu       sync.Mutex
	processingIDs   map[string]string
}

var _ core.Transport = (*Client)(nil)

// New does not connect to Feishu. Start owns the long connection and blocks until
// shutdown; REST calls are available independently. DB is the existing store's
// database. Durable native reply streams require it.
type Adapter = Client

func New(config core.Config, db *sql.DB, log core.Logger, options ...Options) (*Adapter, error) {
	var opts Options
	if len(options) > 1 {
		return nil, errors.New("feishu: one Options value is supported")
	}
	if len(options) == 1 {
		opts = options[0]
	}
	secret := os.Getenv(config.AppSecretEnv)
	if config.AppID == "" || secret == "" {
		return nil, errors.New("feishu: app ID or configured app-secret environment variable is missing")
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = "https://open.feishu.cn"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("feishu: invalid API origin")
	}
	// A custom HTTP origin is an explicit integration/test seam, never read from
	// incoming messages. Production defaults always use HTTPS.
	httpClient := &http.Client{Timeout: 20 * time.Second}
	if opts.HTTPClient != nil {
		copy := *opts.HTTPClient
		httpClient = &copy
		if httpClient.Timeout == 0 {
			httpClient.Timeout = 20 * time.Second
		}
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	if opts.Dialer != nil {
		dialer = *opts.Dialer
	}
	c := &Client{config: config, log: log, db: db, base: base, http: httpClient, dialer: &dialer, secret: secret, botID: opts.BotOpenID, origins: make(map[string]core.Room), processingIDs: make(map[string]string)}
	if c.db != nil {
		_, err = c.db.Exec(`CREATE TABLE IF NOT EXISTS feishu_cards (message TEXT PRIMARY KEY,card TEXT NOT NULL,room TEXT NOT NULL,sequence INTEGER NOT NULL DEFAULT 0,snapshot TEXT NOT NULL)`)
		if err != nil {
			return nil, errors.New("feishu: card-state initialization failed")
		}
		if err = c.initDeliveryState(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func permissionDenied(err error) bool {
	var e *APIError
	return errors.As(err, &e) && !e.Uncertain && !e.DeliveryRetryable() && (e.Status == http.StatusForbidden || e.Code == 99991672)
}
func safeFailure(op string, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return &APIError{Operation: op}
}
func (c *Client) emit(event string, detail core.Args) {
	if c.log != nil {
		c.log(event, detail)
	}
}
func (c *Client) botOpenID() string {
	c.identityMu.RLock()
	defer c.identityMu.RUnlock()
	return c.botID
}
func (c *Client) setBotOpenID(id string) { c.identityMu.Lock(); c.botID = id; c.identityMu.Unlock() }
func (c *Client) isClosed() bool         { c.lifeMu.Lock(); defer c.lifeMu.Unlock(); return c.closed }

const maxJSONBytes = 4 << 20

func readJSON(r io.Reader, dst any, limit int64) error {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return errors.New("response too large")
	}
	return json.Unmarshal(b, dst)
}
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenUntil) {
		return c.token, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": c.config.AppID, "app_secret": c.secret})
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		token, expire, err := c.requestToken(ctx, body)
		if err == nil {
			ttl := min(7200, max(1, expire-60))
			c.token = token
			c.tokenUntil = time.Now().Add(time.Duration(ttl) * time.Second)
			return c.token, nil
		}
		var failure *APIError
		if !errors.As(err, &failure) || !failure.DeliveryRetryable() || attempt+1 >= apiMaxAttempts {
			return "", err
		}
		delay := failure.DeliveryRetryAfter()
		if delay == 0 {
			delay = retryBackoff(attempt)
		}
		if delay > apiMaxInlineWait {
			return "", err
		}
		if e := waitRetry(ctx, delay); e != nil {
			failure.cause = e
			return "", failure
		}
	}
}

func (c *Client) requestToken(ctx context.Context, body []byte) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", 0, safeFailure("auth", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// Refreshing this private credential cannot send a visible message. A
		// connection failure here is safely retryable, unlike message creation.
		return "", 0, &APIError{Operation: "auth", Retryable: true, cause: ctx.Err()}
	}
	defer resp.Body.Close()
	var result struct {
		Code   *int   `json:"code"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	decodeErr := readJSON(resp.Body, &result, 64<<10)
	code := 0
	if result.Code != nil {
		code = *result.Code
	}
	if decodeErr != nil || resp.StatusCode != http.StatusOK || result.Code == nil || code != 0 || result.Token == "" {
		failure := c.responseFailure(req, resp, code, decodeErr == nil && result.Code != nil, false)
		failure.Operation = "auth"
		if code != 99991403 && (resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusOK && (decodeErr != nil || result.Code == nil || code == 0 && result.Token == "")) {
			failure.Retryable = true
			failure.RetryAfter = responseRetryAfter(resp.Header, time.Now())
		}
		return "", 0, failure
	}
	return result.Token, result.Expire, nil
}

// api retries only explicit, temporary API rejections. Network failures and
// ambiguous responses never transparently replay a mutation. A serialized JSON
// body is reused verbatim; streamed uploads use doAPI once instead.
func (c *Client) api(ctx context.Context, method, path string, query url.Values, body any, dst any) error {
	if c.isClosed() {
		return errors.New("feishu: transport is closed")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var data []byte
	if body != nil {
		var e error
		data, e = json.Marshal(body)
		if e != nil {
			return errors.New("feishu: invalid request")
		}
	}
	target := c.base + "/open-apis" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
		if e != nil {
			return safeFailure("request", e)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		err = c.doAPI(req, dst, method != http.MethodGet && method != http.MethodHead)
		var failure *APIError
		if !errors.As(err, &failure) || !failure.DeliveryRetryable() || attempt+1 >= apiMaxAttempts {
			return err
		}
		delay := failure.RetryAfter
		if delay <= 0 {
			delay = retryBackoff(attempt)
		}
		// Longer provider backoffs belong in the durable gateway queue, not in a
		// sleeping render or an HTTP handler. Never shorten a provider deadline.
		if delay > apiMaxInlineWait {
			return err
		}
		if e = waitRetry(ctx, delay); e != nil {
			// The last attempt was explicitly rejected. Cancellation while waiting
			// must not turn a known non-write into an uncertain write.
			failure.cause = e
			return failure
		}
	}
}
func (c *Client) doAPI(req *http.Request, dst any, mutation bool) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return &APIError{Operation: requestOperation(req), Uncertain: mutation, cause: req.Context().Err()}
	}
	defer resp.Body.Close()
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
		Bot  json.RawMessage `json:"bot"`
	}
	decodeErr := readJSON(resp.Body, &envelope, maxJSONBytes)
	code := 0
	if envelope.Code != nil {
		code = *envelope.Code
	}
	if decodeErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Code == nil || code != 0 {
		// Invalid/expired token can be refreshed on a later independent operation.
		if code == 99991663 || code == 99991668 || resp.StatusCode == 401 {
			c.tokenMu.Lock()
			c.token = ""
			c.tokenUntil = time.Time{}
			c.tokenMu.Unlock()
		}
		return c.responseFailure(req, resp, code, decodeErr == nil && envelope.Code != nil, mutation)
	}
	if dst != nil {
		data := envelope.Data
		if len(data) == 0 && len(envelope.Bot) > 0 {
			data = envelope.Bot
		}
		if len(data) == 0 || json.Unmarshal(data, dst) != nil {
			return &APIError{Operation: "response", Status: resp.StatusCode, Uncertain: mutation, RequestID: c.responseRequestID(req, resp)}
		}
	}
	return nil
}

// keyedLocks drops unused entries, so idle chats and old card IDs do not grow a
// process-wide lock map. All card updates for a message are serialized.
type keyedLocks struct {
	mu    sync.Mutex
	items map[string]*keyedLock
}
type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.items == nil {
		k.items = map[string]*keyedLock{}
	}
	entry := k.items[key]
	if entry == nil {
		entry = &keyedLock{}
		k.items[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.items, key)
		}
		k.mu.Unlock()
	}
}

func validID(s string) bool { return s != "" && len(s) <= 4096 && !strings.ContainsAny(s, "\r\n\x00") }
func escapeID(s string) (string, error) {
	if !validID(s) || s == "." || s == ".." || strings.ContainsAny(s, "/\\?#") {
		return "", errors.New("feishu: invalid identifier")
	}
	return url.PathEscape(s), nil
}
func stringValue(a map[string]any, k string) string { s, _ := a[k].(string); return s }
