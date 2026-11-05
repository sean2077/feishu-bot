package feishu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestAPIRejectionClassificationAndSafeRequestID(t *testing.T) {
	for _, tc := range []struct {
		name string
		status, code int
		retryable, uncertain bool
	}{
		{"rate-limit", 429, 99991400, true, false},
		{"legacy-rate-limit", 400, 99991400, true, false},
		{"monthly-quota", 429, 99991403, false, false},
		{"permission", 403, 99991672, false, false},
		{"unknown-code", 200, 1250000, false, false},
		{"upstream-failure", 503, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "10")
				w.Header().Set("X-Tt-Logid", "trace_123-ABC")
				jsonResponse(w, tc.status, core.Args{"code": tc.code, "msg": "fixture-app-secret fixture-token"})
			})
			_, err := c.Send(context.Background(), fixtureRoom(t, c), "hello", "", "")
			var failure *APIError
			if !errors.As(err, &failure) || failure.DeliveryRetryable() != tc.retryable || failure.Uncertain != tc.uncertain || calls != 1 {
				t.Fatalf("classification/calls: error=%v calls=%d", err, calls)
			}
			if failure.RequestID != "trace_123-ABC" || strings.Contains(err.Error(), "fixture-token") || strings.Contains(err.Error(), "fixture-app-secret") {
				t.Fatalf("unsafe diagnostics: %v", err)
			}
			if tc.retryable && failure.RetryAfter != 10*time.Second {
				t.Fatalf("provider deadline was shortened: %v", failure.RetryAfter)
			}
		})
	}
}

func TestDefiniteRateLimitRetriesReuseExactMessageUUID(t *testing.T) {
	var bodies []string
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if len(bodies) < 3 {
			jsonResponse(w, 429, core.Args{"code": 99991400})
			return
		}
		apiResponse(w, core.Args{"message_id": "om_once"})
	})
	id, err := c.Send(context.Background(), fixtureRoom(t, c), "same body", "", "")
	if err != nil || id != "om_once" || len(bodies) != 3 || bodies[0] != bodies[1] || bodies[1] != bodies[2] || !strings.Contains(bodies[0], `"uuid":"`) {
		t.Fatalf("bounded immutable retry failed: id=%q error=%v bodies=%v", id, err, bodies)
	}
}

func TestCancellationDuringRetryWaitKeepsDefiniteRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		calls++
		jsonResponse(w, 429, core.Args{"code": 99991400})
		// Wait until the HTTP client has received the rejection, then its caller
		// cancels via the response-body seam below, before the backoff expires.
	})
	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.http.Transport = deliveryRoundTripper(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err == nil && response.StatusCode == 429 {
			response.Body = &cancelAfterRead{ReadCloser: response.Body, cancel: cancel}
		}
		return response, err
	})
	_, err := c.Send(ctx, fixtureRoom(t, c), "cancel", "", "")
	var failure *APIError
	if !errors.As(err, &failure) || failure.Uncertain || !failure.Retryable || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel changed confirmed rejection: %v calls=%d", err, calls)
	}
}

type deliveryRoundTripper func(*http.Request) (*http.Response, error)
func (f deliveryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
type cancelAfterRead struct { io.ReadCloser; cancel context.CancelFunc }
func (r *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF { r.cancel() }
	return n, err
}

func TestMessageJournalSurvivesRestartAndRequiresExplicitUnknownRecovery(t *testing.T) {
	db := fixtureDB(t)
	var bodies []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if len(bodies) == 1 {
			jsonResponse(w, 503, core.Args{"code": 1})
			return
		}
		apiResponse(w, core.Args{"message_id": "om_recovered"})
	}
	c, _ := fixtureClient(t, db, handler)
	ctx := core.WithDeliveryID(context.Background(), "outbox:restart")
	room := fixtureRoom(t, c)
	if _, err := c.Send(ctx, room, "durable", "", ""); err == nil {
		t.Fatal("unknown outcome was accepted")
	}
	// Simulate a restart after the provider's deduplication window. This must
	// remain blocked until the operator explicitly requests recovery.
	if _, err := db.Exec(`UPDATE feishu_deliveries SET first_attempt=? WHERE id=?`, time.Now().Add(-2*time.Hour).UnixMilli(), core.DeliveryID(ctx)); err != nil { t.Fatal(err) }
	c2, _ := fixtureClient(t, db, handler)
	if _, err := c2.Send(ctx, room, "durable", "", ""); err == nil || len(bodies) != 1 {
		t.Fatal("restart automatically replayed an uncertain write")
	}
	if _, err := c2.Send(core.WithExplicitRetry(ctx), room, "changed", "", ""); err == nil || len(bodies) != 1 {
		t.Fatal("logical delivery accepted a different payload")
	}
	id, err := c2.Send(core.WithExplicitRetry(ctx), room, "durable", "", "")
	if err != nil || id != "om_recovered" || len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("explicit retry changed UUID/body: %v %v", err, bodies)
	}
	if again, err := c2.Send(ctx, room, "durable", "", ""); err != nil || again != id || len(bodies) != 2 {
		t.Fatalf("confirmed journal was sent again: %q %v", again, err)
	}
}

func TestCardCreationAndMessageAssociationRecoverIndependently(t *testing.T) {
	db := fixtureDB(t)
	creates, sends := 0, 0
	var uuids []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		body := decodedBody(t, r)
		if strings.Contains(r.URL.Path, "cardkit") {
			creates++
			if _, exists := body["uuid"]; exists { t.Error("CardKit create has no UUID field") }
			apiResponse(w, core.Args{"card_id": "card_saved"})
			return
		}
		sends++
		uuids = append(uuids, stringValue(body, "uuid"))
		if sends == 1 {
			w.Header().Set("Retry-After", "10")
			jsonResponse(w, 429, core.Args{"code": 99991400})
			return
		}
		apiResponse(w, core.Args{"message_id": "om_card"})
	}
	c, _ := fixtureClient(t, db, handler)
	ctx := core.WithDeliveryID(context.Background(), "render:partial-create")
	room := fixtureRoom(t, c)
	if _, err := c.Card(ctx, room, replyFixture("frozen", false), "", ""); err == nil { t.Fatal("rate limit accepted") }
	c2, _ := fixtureClient(t, db, handler)
	if _, err := c2.Card(ctx, room, replyFixture("frozen", false), "", ""); err == nil || creates != 1 || sends != 1 { t.Fatal("provider deadline ignored") }
	if _, err := db.Exec(`UPDATE feishu_deliveries SET retry_at=0 WHERE id=? AND step='message'`, core.DeliveryID(ctx)); err != nil { t.Fatal(err) }
	id, err := c2.Card(ctx, room, replyFixture("frozen", false), "", "")
	if err != nil || id != "om_card" || creates != 1 || sends != 2 || uuids[0] == "" || uuids[0] != uuids[1] {
		t.Fatalf("card entity was recreated: %q %v creates=%d sends=%d UUIDs=%v", id, err, creates, sends, uuids)
	}
}

func TestFileJournalReusesUploadAndMessageUUIDAcrossRestart(t *testing.T) {
	db := fixtureDB(t)
	path := filepath.Join(t.TempDir(), "export.md")
	if err := os.WriteFile(path, []byte("frozen file bytes"), 0600); err != nil { t.Fatal(err) }
	uploads, sends := 0, 0
	var bodies []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			uploads++
			if err := r.ParseMultipartForm(1 << 20); err != nil { t.Error(err) }
			if r.MultipartForm != nil { defer r.MultipartForm.RemoveAll() }
			apiResponse(w, core.Args{"file_key": "file_saved"})
			return
		}
		sends++
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if sends == 1 { jsonResponse(w, 503, core.Args{"code": 1}); return }
		apiResponse(w, core.Args{"message_id": "om_file"})
	}
	c, _ := fixtureClient(t, db, handler)
	ctx := core.WithDeliveryID(context.Background(), "outbox:file")
	room := fixtureRoom(t, c)
	if _, err := c.File(ctx, room, path, "export.md"); err == nil { t.Fatal("unknown file send accepted") }
	c2, _ := fixtureClient(t, db, handler)
	if _, err := c2.File(ctx, room, path, "export.md"); err == nil || uploads != 1 || sends != 1 { t.Fatal("file automatically replayed") }
	id, err := c2.File(core.WithExplicitRetry(ctx), room, path, "export.md")
	if err != nil || id != "om_file" || uploads != 1 || sends != 2 || bodies[0] != bodies[1] || !strings.Contains(bodies[0], "file_saved") {
		t.Fatalf("file upload/UUID changed: %q %v uploads=%d sends=%d", id, err, uploads, sends)
	}
}

func TestConcurrentLogicalSendCommitsOneMessage(t *testing.T) {
	db := fixtureDB(t)
	var mu sync.Mutex
	calls := 0
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock(); calls++; mu.Unlock()
		apiResponse(w, core.Args{"message_id": "om_single"})
	})
	ctx := core.WithDeliveryID(context.Background(), "outbox:concurrent")
	room := fixtureRoom(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); id, err := c.Send(ctx, room, "same", "", ""); if err != nil || id != "om_single" { t.Errorf("send %q: %v", id, err) } }()
	}
	wg.Wait()
	mu.Lock(); defer mu.Unlock()
	if calls != 1 { t.Fatalf("logical send emitted %d messages", calls) }
}
