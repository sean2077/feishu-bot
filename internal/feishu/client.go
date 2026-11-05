// Package feishu provides the bounded Feishu REST and long-connection transport.
package feishu

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	}
	return c, nil
}

// APIError deliberately excludes provider text, URLs, request bodies and tokens.
// Uncertain indicates that a mutation must not be blindly repeated.
type APIError struct {
	Operation    string
	Status, Code int
	Uncertain    bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("feishu: %s failed (HTTP %d, code %d, uncertain %t)", e.Operation, e.Status, e.Code, e.Uncertain)
}
func permissionDenied(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == http.StatusForbidden || e.Code == 99991672)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", safeFailure("auth", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", safeFailure("auth", err)
	}
	defer resp.Body.Close()
	var result struct {
		Code   int    `json:"code"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err = readJSON(resp.Body, &result, 64<<10); err != nil || resp.StatusCode != http.StatusOK || result.Code != 0 || result.Token == "" {
		return "", &APIError{Operation: "auth", Status: resp.StatusCode, Code: result.Code}
	}
	ttl := result.Expire - 60
	if ttl < 1 {
		ttl = 1
	}
	if ttl > 7200 {
		ttl = 7200
	}
	c.token = result.Token
	c.tokenUntil = time.Now().Add(time.Duration(ttl) * time.Second)
	return c.token, nil
}

// api sends each operation once. In particular, a token error or lost response
// never transparently replays a message, upload, reaction or card creation.
func (c *Client) api(ctx context.Context, method, path string, query url.Values, body any, dst any) error {
	if c.isClosed() {
		return errors.New("feishu: transport is closed")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, e := json.Marshal(body)
		if e != nil {
			return errors.New("feishu: invalid request")
		}
		reader = bytes.NewReader(data)
	}
	target := c.base + "/open-apis" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return safeFailure("request", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return c.doAPI(req, dst, method != http.MethodGet && method != http.MethodHead)
}
func (c *Client) doAPI(req *http.Request, dst any, mutation bool) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return &APIError{Operation: "request", Uncertain: mutation}
	}
	defer resp.Body.Close()
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
		Bot  json.RawMessage `json:"bot"`
	}
	if err = readJSON(resp.Body, &envelope, maxJSONBytes); err != nil {
		return &APIError{Operation: "response", Status: resp.StatusCode, Uncertain: mutation}
	}
	code := 0
	if envelope.Code != nil {
		code = *envelope.Code
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Code == nil || code != 0 {
		// Invalid/expired token can be refreshed on a later independent operation.
		if code == 99991663 || code == 99991668 || resp.StatusCode == 401 {
			c.tokenMu.Lock()
			c.token = ""
			c.tokenUntil = time.Time{}
			c.tokenMu.Unlock()
		}
		return &APIError{Operation: "API", Status: resp.StatusCode, Code: code, Uncertain: mutation && (resp.StatusCode >= 500 || envelope.Code == nil)}
	}
	if dst != nil {
		data := envelope.Data
		if len(data) == 0 && len(envelope.Bot) > 0 {
			data = envelope.Bot
		}
		if len(data) == 0 || json.Unmarshal(data, dst) != nil {
			return &APIError{Operation: "response", Status: resp.StatusCode, Uncertain: mutation}
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
