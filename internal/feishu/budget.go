package feishu

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const (
	groupAuth = iota
	groupEndpoint
	groupCards
	groupMessages
	groupFiles
	groupResources
	groupOther
	httpGroupCount

	httpActiveLimit = 8
	httpWaitingLimit = 64
	httpPreviewLimit = 2
)

var httpGroupNames = [httpGroupCount]string{"auth", "endpoint", "cards", "messages", "files", "resources", "other"}
var httpGroupLimits = [httpGroupCount]int{2, 2, 4, 4, 2, 2, 4}

// requestGroup uses a fixed vocabulary, never a message, card, room or URL as a
// map key. Preview writes cannot exhaust either the application or card budget.
func requestGroup(req *http.Request) (int, bool) {
	path := req.URL.Path
	switch {
	case strings.HasPrefix(path, "/open-apis/auth/"):
		return groupAuth, false
	case path == "/callback/ws/endpoint":
		return groupEndpoint, false
	case strings.HasPrefix(path, "/open-apis/cardkit/"):
		return groupCards, strings.HasSuffix(path, "/content")
	case strings.Contains(path, "/resources/"):
		return groupResources, false
	case path == "/open-apis/im/v1/files":
		return groupFiles, false
	case strings.HasPrefix(path, "/open-apis/im/v1/messages"):
		return groupMessages, false
	default:
		return groupOther, false
	}
}

type requestBudget struct {
	mu sync.Mutex
	changed chan struct{}
	closed bool
	active, waiting, previews, peak int
	groups [httpGroupCount]int
	until [httpGroupCount]time.Time
	admitted, rejected, limited uint64
	waitNanos, activeNanos uint64
}

func (b *requestBudget) signal() {
	if b.changed != nil {
		close(b.changed)
	}
	b.changed = make(chan struct{})
}

func (b *requestBudget) acquire(ctx context.Context, group int, preview bool) (func(), error) {
	started := time.Now()
	waiting := false
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() {
		if waiting {
			b.waiting--
		}
		b.waitNanos += uint64(time.Since(started))
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, &APIError{Operation: "request admission", Retryable: true, cause: err}
		}
		if b.closed {
			return nil, &APIError{Operation: "request admission", Retryable: true, cause: context.Canceled}
		}
		if delay := time.Until(b.until[group]); delay > 0 {
			b.rejected++
			return nil, &APIError{Operation: "shared rate limit", Retryable: true, RetryAfter: delay}
		}
		if b.active < httpActiveLimit && b.groups[group] < httpGroupLimits[group] && (!preview || b.previews < httpPreviewLimit) {
			b.active++
			b.groups[group]++
			if preview {
				b.previews++
			}
			b.peak = max(b.peak, b.active)
			b.admitted++
			admittedAt := time.Now()
			var once sync.Once
			return func() {
				once.Do(func() {
					b.mu.Lock()
					b.active--
					b.groups[group]--
					if preview {
						b.previews--
					}
					b.activeNanos += uint64(time.Since(admittedAt))
					b.signal()
					b.mu.Unlock()
				})
			}, nil
		}
		if !waiting {
			if b.waiting >= httpWaitingLimit {
				b.rejected++
				return nil, &APIError{Operation: "request capacity", Retryable: true, RetryAfter: 250 * time.Millisecond}
			}
			waiting = true
			b.waiting++
		}
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		b.mu.Lock()
	}
}

func (b *requestBudget) rateLimited(req *http.Request, delay time.Duration) {
	group, _ := requestGroup(req)
	if delay <= 0 {
		// Per-operation retries already add jitter and exponential delay. Use
		// their minimum here so this shared floor cannot consume an attempt
		// solely because two independently sampled jitters disagreed.
		delay = 250 * time.Millisecond
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	until := time.Now().Add(delay)
	if until.After(b.until[group]) {
		b.until[group] = until
	}
	b.limited++
	b.signal()
}

func (b *requestBudget) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.signal()
}

func (b *requestBudget) snapshot() core.Args {
	b.mu.Lock()
	defer b.mu.Unlock()
	groups := core.Args{}
	for i, name := range httpGroupNames {
		groups[name] = core.Args{"active": b.groups[i], "limit": httpGroupLimits[i], "backoffMs": max(int64(0), time.Until(b.until[i]).Milliseconds())}
	}
	return core.Args{"active": b.active, "activeLimit": httpActiveLimit, "peakActive": b.peak, "waiting": b.waiting, "waitingLimit": httpWaitingLimit, "previews": b.previews, "previewLimit": httpPreviewLimit, "admitted": b.admitted, "rejectedBeforeSend": b.rejected, "rateLimited": b.limited, "waitMs": b.waitNanos / uint64(time.Millisecond), "activeMs": b.activeNanos / uint64(time.Millisecond), "groups": groups}
}

type budgetBody struct {
	io.ReadCloser
	release func()
}

func (b *budgetBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// doHTTP holds admission until the response body is closed, so a streamed
// download/upload remains part of the real in-flight budget. It never retries.
func (c *Client) doHTTP(req *http.Request, client *http.Client) (*http.Response, error) {
	group, preview := requestGroup(req)
	release, err := c.budget.acquire(req.Context(), group, preview)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		release()
		return nil, err
	}
	resp.Body = &budgetBody{ReadCloser: resp.Body, release: release}
	if resp.Request == nil {
		resp.Request = req
	}
	return resp, nil
}

// Diagnostics is a bounded, content-free snapshot. A connected socket is not a
// claim that the model or a particular delivery is healthy.
func (c *Client) Diagnostics() core.Args {
	return core.Args{"http": c.budget.snapshot(), "websocket": c.wsStats.snapshot()}
}
