package feishu

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const (
	apiMaxAttempts   = 3
	apiMaxInlineWait = 2 * time.Second
)

// APIError excludes provider text, URLs, request bodies and credentials.
// Retryable is set only for explicit temporary rejections. Uncertain wins over
// Retryable and requires user-authorized recovery for a new external write.
type APIError struct {
	Operation    string
	Status, Code int
	Uncertain    bool
	Retryable    bool
	RetryAfter   time.Duration
	RequestID    string
	cause        error
}

var _ core.DeliveryFailure = (*APIError)(nil)

func (e *APIError) Error() string {
	detail := fmt.Sprintf("feishu: %s failed (HTTP %d, code %d, uncertain %t, retryable %t)", e.Operation, e.Status, e.Code, e.Uncertain, e.DeliveryRetryable())
	if e.RequestID != "" {
		detail += " request_id=" + e.RequestID
	}
	return detail
}

func (e *APIError) Unwrap() error                     { return e.cause }
func (e *APIError) DeliveryRetryable() bool           { return e.Retryable && !e.Uncertain }
func (e *APIError) DeliveryUncertain() bool           { return e.Uncertain }
func (e *APIError) DeliveryRetryAfter() time.Duration { return max(time.Duration(0), e.RetryAfter) }

func localRejection(operation string, err error) error {
	if err == nil {
		return nil
	}
	var failure core.DeliveryFailure
	if errors.As(err, &failure) {
		return err
	}
	return &APIError{Operation: operation, cause: err}
}

func requestOperation(req *http.Request) string {
	path := req.URL.Path
	switch {
	case strings.HasPrefix(path, "/open-apis/cardkit/v1/cards"):
		if req.Method == http.MethodPost {
			return "card create"
		}
		if strings.HasSuffix(path, "/content") {
			return "card content update"
		}
		return "card update"
	case path == "/open-apis/im/v1/files":
		return "file upload"
	case strings.HasPrefix(path, "/open-apis/im/v1/messages"):
		if req.Method == http.MethodPost {
			return "message send"
		}
		if req.Method == http.MethodPatch {
			return "message update"
		}
		return "message read"
	default:
		return "API"
	}
}

func (c *Client) responseFailure(req *http.Request, resp *http.Response, code int, structured, mutation bool) *APIError {
	e := &APIError{Operation: requestOperation(req), Status: resp.StatusCode, Code: code, RequestID: c.responseRequestID(req, resp)}
	switch {
	case code == 99991403:
		// Monthly quota is also HTTP 429. Repeating it cannot help and must not
		// produce a background retry loop.
	case resp.StatusCode == http.StatusTooManyRequests || code == 99991400:
		e.Retryable = true
		e.RetryAfter = responseRetryAfter(resp.Header, time.Now())
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout:
		e.Uncertain = mutation
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Authentication, permission and malformed requests are confirmed
		// rejections. A non-JSON error page does not make them a lost success.
	case !structured:
		e.Uncertain = mutation
	}
	return e
}

func responseRetryAfter(headers http.Header, now time.Time) time.Duration {
	// Feishu uses x-ogw-ratelimit-reset for both HTTP 429 and legacy 400
	// code 99991400 responses. Honor the longer delay if both are supplied.
	var delay time.Duration
	for _, key := range []string{"Retry-After", "X-Ogw-Ratelimit-Reset"} {
		raw := strings.TrimSpace(headers.Get(key))
		if raw == "" {
			continue
		}
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if seconds > int64((1<<63-1)/time.Second) {
				return time.Duration(1<<63 - 1)
			}
			if seconds > 0 {
				delay = max(delay, time.Duration(seconds)*time.Second)
			}
			continue
		}
		if key == "Retry-After" {
			if at, err := http.ParseTime(raw); err == nil && at.After(now) {
				delay = max(delay, at.Sub(now))
			}
		}
	}
	return delay
}

func (c *Client) responseRequestID(req *http.Request, resp *http.Response) string {
	for _, header := range []string{"X-Tt-Logid", "X-Request-Id", "X-Lark-Request-Id"} {
		value := strings.TrimSpace(resp.Header.Get(header))
		if value == "" || len(value) > 128 {
			continue
		}
		valid := true
		for _, char := range value {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
				valid = false
				break
			}
		}
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if valid && !(c.secret != "" && strings.Contains(value, c.secret)) && !(token != "" && strings.Contains(value, token)) {
			return value
		}
	}
	return ""
}

func retryBackoff(attempt int) time.Duration {
	delay := 250 * time.Millisecond * time.Duration(1<<min(attempt, 3))
	return delay + time.Duration(rand.Int64N(int64(delay/4)))
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
