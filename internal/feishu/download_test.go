package feishu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestDownloadSharesItsTotalDeadlineWithAuthorizationAndEveryBodyRequest(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/resources/") {
			_, _ = io.WriteString(w, "complete")
			return
		}
		apiResponse(w, core.Args{"items": []any{fixtureItem("om_deadline", "oc_allowed", "ou_member", "file", `{"file_key":"file_deadline","file_name":"data.bin"}`)}})
	})
	c.rememberOrigin("om_deadline", fixtureRoom(t, c))
	// Remove the independent, shorter per-request client timeout from this
	// observation so the RoundTripper sees the enclosing download deadline.
	c.http.Timeout = 2 * time.Minute
	base := c.http.Transport
	if base == nil { base = http.DefaultTransport }
	var mu sync.Mutex
	deadlines := map[string]time.Time{}
	c.http.Transport = deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > 60*time.Second { t.Error("download request escaped its total deadline", req.URL.Path) }
		mu.Lock()
		deadlines[req.URL.Path] = deadline
		mu.Unlock()
		return base.RoundTrip(req)
	})
	path := filepath.Join(t.TempDir(), "downloaded")
	if err := c.Download(context.Background(), "om_deadline", core.Resource{Kind: "file", Key: "file_deadline"}, path, 100); err != nil { t.Fatal(err) }
	mu.Lock()
	defer mu.Unlock()
	auth := deadlines["/open-apis/auth/v3/tenant_access_token/internal"]
	if auth.IsZero() || len(deadlines) != 3 { t.Fatal("authorization and resource preflight were not included", deadlines) }
	for _, deadline := range deadlines {
		if !deadline.Equal(auth) { t.Fatal("download reset its timeout after authorization", deadlines) }
	}
}

func TestDownloadUsesOrdinaryGETThenBoundedValidatedRanges(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprint("range=", ranged), func(t *testing.T) {
			payload := []byte(`{"code":234037,"msg":"this is a user JSON file"}`)
			if ranged {
				payload = bytes.Repeat([]byte("range-content\x00"), int(downloadRangeBytes*2/14)+1)
			}
			var resources atomic.Int32
			c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/resources/") {
					apiResponse(w, core.Args{"items": []any{fixtureItem("om_range", "oc_allowed", "ou_member", "file", `{"file_key":"file_range","file_name":"data.json"}`)}})
					return
				}
				resources.Add(1)
				if r.Header.Get("Range") == "" {
					if ranged {
						jsonResponse(w, 400, core.Args{"code": 234037, "msg": "download with Range"})
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(payload)
					}
					return
				}
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || end-start >= downloadRangeBytes || start < 0 || end < start || start >= int64(len(payload)) {
					t.Error("invalid outbound range", r.Header.Get("Range"))
					http.Error(w, "invalid", 400)
					return
				}
				if start > 0 && r.Header.Get("If-Range") != `"fixture-version"` {
					t.Error("resumed range did not bind its validator")
				}
				end = min(end, int64(len(payload))-1)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
				w.Header().Set("ETag", `"fixture-version"`)
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(payload[start : end+1])
			})
			c.config.Limits.AttachmentBytes = 20 << 20
			c.rememberOrigin("om_range", fixtureRoom(t, c))
			path := filepath.Join(t.TempDir(), "received.json")
			err := c.Download(context.Background(), "om_range", core.Resource{Kind: "file", Key: "file_range"}, path, 20<<20)
			actual, readErr := os.ReadFile(path)
			if err != nil || readErr != nil || !bytes.Equal(actual, payload) {
				t.Fatalf("download did not preserve bytes: download=%v read=%v", err, readErr)
			}
			want := int32(1)
			if ranged {
				want += int32((int64(len(payload)) + downloadRangeBytes - 1) / downloadRangeBytes)
			}
			if resources.Load() != want || c.budget.snapshot()["active"] != 0 {
				t.Fatalf("unbounded requests or retained admission: requests=%d want=%d diagnostics=%v", resources.Load(), want, c.Diagnostics())
			}
		})
	}
}

func TestDownloadRejectsInvalidRangesAndRemovesEveryPartial(t *testing.T) {
	for _, name := range []string{"missing", "unknown-total", "start", "end", "oversize", "truncated", "extra", "encoded", "changed-length", "changed-etag", "ignored-resume"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			large := strings.HasPrefix(name, "changed-") || name == "ignored-resume"
			limit := int64(16)
			if large {
				limit = downloadRangeBytes + 16
			}
			c, server := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 1 {
					jsonResponse(w, 400, core.Args{"code": 234037})
					return
				}
				if large && n == 2 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", downloadRangeBytes-1, limit-1))
					w.Header().Set("ETag", `"original"`)
					w.WriteHeader(206)
					_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", int(downloadRangeBytes))), downloadRangeBytes)
					return
				}
				contentRange := "bytes 0-15/16"
				body := strings.Repeat("x", 16)
				if large {
					w.Header().Set("ETag", `"original"`)
				}
				switch name {
				case "missing":
					contentRange = ""
				case "unknown-total":
					contentRange = "bytes 0-15/*"
				case "start":
					contentRange = "bytes 1-15/16"
				case "end":
					contentRange = "bytes 0-14/16"
				case "oversize":
					contentRange = "bytes 0-15/17"
				case "truncated":
					body = "short"
				case "extra":
					body += "x"
				case "encoded":
					w.Header().Set("Content-Encoding", "gzip")
				case "changed-length":
					contentRange = fmt.Sprintf("bytes %d-%d/%d", downloadRangeBytes, limit-2, limit)
				case "changed-etag":
					contentRange = fmt.Sprintf("bytes %d-%d/%d", downloadRangeBytes, limit-2, limit-1)
					w.Header().Set("ETag", `"replacement"`)
				case "ignored-resume":
					_, _ = io.WriteString(w, body)
					return
				}
				w.Header().Set("Content-Range", contentRange)
				w.WriteHeader(206)
				w.(http.Flusher).Flush() // Do not let net/http synthesize a matching Content-Length.
				_, _ = io.WriteString(w, body)
			})
			path := filepath.Join(t.TempDir(), "partial")
			err := c.downloadResource(context.Background(), server.URL+"/open-apis/im/v1/messages/om_x/resources/file_x", "fixture-token", path, limit)
			if err == nil {
				t.Fatal("invalid range accepted")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("failed partial resource retained")
			}
			if calls.Load() > 3 || c.budget.snapshot()["active"] != 0 {
				t.Fatal("failed ranges retried or retained admission", calls.Load(), c.Diagnostics())
			}
		})
	}
}

func TestDownloadAcceptsInitialIgnoredRangeAndCancelsStream(t *testing.T) {
	for _, cancelBody := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelBody), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			c, server := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					jsonResponse(w, 400, core.Args{"code": 234037})
					return
				}
				if cancelBody {
					w.Header().Set("Content-Range", "bytes 0-15/16")
					w.WriteHeader(206)
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, "start")
					w.(http.Flusher).Flush()
					cancel()
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, "whole")
			})
			path := filepath.Join(t.TempDir(), "received")
			err := c.downloadResource(ctx, server.URL+"/open-apis/im/v1/messages/om_x/resources/file_x", "fixture-token", path, 16)
			if cancelBody {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("cancelled range retained")
				}
			} else {
				data, readErr := os.ReadFile(path)
				if err != nil || readErr != nil || string(data) != "whole" {
					t.Fatal("ignored initial Range was not bounded as a whole response", err, readErr)
				}
			}
		})
	}
}

func TestDownloadErrorIsBoundedAndRedactsProviderContent(t *testing.T) {
	var calls atomic.Int32
	c, server := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Tt-Logid", "safe-request-123")
		jsonResponse(w, 403, core.Args{"code": 234004, "msg": "fixture-app-secret fixture-token private-file-body"})
	})
	err := c.downloadResource(context.Background(), server.URL+"/open-apis/im/v1/messages/om_x/resources/file_x", "fixture-token", filepath.Join(t.TempDir(), "denied"), 16)
	var failure *APIError
	if !errors.As(err, &failure) || failure.Code != 234004 || failure.RequestID != "safe-request-123" || failure.Uncertain || calls.Load() != 1 {
		t.Fatal("download rejection lost safe API metadata", err)
	}
	for _, secret := range []string{"fixture-app-secret", "fixture-token", "private-file-body", "file_x", server.URL} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("download error leaked provider content")
		}
	}
	for _, invalid := range []string{"bytes +0-1/2", "bytes 0-1/0", "bytes 0-1/9223372036854775808", "bytes 1-0/2", "bytes 0-2/2", "bytes 0-1/2junk", "items 0-1/2"} {
		if _, _, _, valid := parseContentRange(invalid); valid {
			t.Fatal("invalid content range accepted", invalid)
		}
	}
}
