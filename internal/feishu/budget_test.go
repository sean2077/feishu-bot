package feishu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func budgetRequest(ctx context.Context, path string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "https://open.feishu.cn/open-apis/"+path, strings.NewReader(`{}`))
	return req
}

func awaitBudgetResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result: return err
	case <-time.After(3*time.Second): t.Fatal("HTTP admission did not finish"); return nil
	}
}

func TestOutboundPreviewsReserveCapacityForFinalAndReleaseCancelledWaiters(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("unexpected fixture HTTP") })
	entered := make(chan string, 4)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	c.http.Transport = deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		entered <- req.URL.Path
		select { case <-release: case <-req.Context().Done(): return nil, req.Context().Err() }
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0}`))}, nil
	})
	results := make(chan error, 3)
	for i := 0; i < httpPreviewLimit; i++ {
		go func() { results <- c.doAPI(budgetRequest(context.Background(), "cardkit/v1/cards/card_preview/elements/reply_answer/content"), nil, true) }()
	}
	for i := 0; i < httpPreviewLimit; i++ {
		select { case <-entered: case <-time.After(3*time.Second): t.Fatal("preview did not enter transport") }
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := make(chan error, 1)
	go func() { queued <- c.doAPI(budgetRequest(ctx, "cardkit/v1/cards/card_queued/elements/reply_answer/content"), nil, true) }()
	go func() { results <- c.doAPI(budgetRequest(context.Background(), "cardkit/v1/cards/card_final"), nil, true) }()
	select {
	case path := <-entered:
		if path != "/open-apis/cardkit/v1/cards/card_final" { t.Fatal("previews exhausted reserved final capacity", path) }
	case <-time.After(3*time.Second): t.Fatal("final card blocked by previews")
	}
	cancel()
	err := awaitBudgetResult(t, queued)
	var failure core.DeliveryFailure
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.DeliveryUncertain() {
		t.Fatal("cancelled admission was misclassified as an external write", err)
	}
	unblock()
	for i := 0; i < httpPreviewLimit+1; i++ { if err := awaitBudgetResult(t, results); err != nil { t.Fatal(err) } }
	if stats := c.budget.snapshot(); stats["active"] != 0 || stats["waiting"] != 0 || stats["previews"] != 0 || stats["peakActive"] != 3 {
		t.Fatal("admission was not released", stats)
	}
}

func TestOutboundApplicationAndWaitingLimitsRejectBeforeWriting(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("unexpected fixture HTTP") })
	entered := make(chan struct{}, httpActiveLimit+1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	c.http.Transport = deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-release
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0}`))}, nil
	})
	active := make(chan error, httpActiveLimit)
	for i := 0; i < httpActiveLimit; i++ {
		path := "cardkit/v1/cards/card_fixture"
		if i < 4 { path = "im/v1/messages/om_fixture" }
		go func(path string) { active <- c.doAPI(budgetRequest(context.Background(), path), nil, true) }(path)
	}
	for i := 0; i < httpActiveLimit; i++ {
		select { case <-entered: case <-time.After(3*time.Second): t.Fatal("application budget did not fill") }
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, httpWaitingLimit)
	for i := 0; i < httpWaitingLimit; i++ {
		go func() { waiting <- c.doAPI(budgetRequest(ctx, "bot/v3/info"), nil, true) }()
	}
	deadline := time.NewTimer(3*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for c.budget.snapshot()["waiting"] != httpWaitingLimit {
		select { case <-tick.C: case <-deadline.C: t.Fatal("bounded admission queue did not fill") }
	}
	rejected := make(chan error, 1)
	go func() { rejected <- c.doAPI(budgetRequest(ctx, "bot/v3/info"), nil, true) }()
	err := awaitBudgetResult(t, rejected)
	var failure core.DeliveryFailure
	if !errors.As(err, &failure) || !failure.DeliveryRetryable() || failure.DeliveryUncertain() || failure.DeliveryRetryAfter() <= 0 {
		t.Fatal("capacity rejection lost definite no-write semantics", err)
	}
	cancel()
	for i := 0; i < httpWaitingLimit; i++ { if err := awaitBudgetResult(t, waiting); !errors.Is(err, context.Canceled) { t.Fatal(err) } }
	select { case <-entered: t.Fatal("queued request reached the network"); default: }
	unblock()
	for i := 0; i < httpActiveLimit; i++ { if err := awaitBudgetResult(t, active); err != nil { t.Fatal(err) } }
	if stats := c.budget.snapshot(); stats["active"] != 0 || stats["waiting"] != 0 || stats["peakActive"] != httpActiveLimit || stats["rejectedBeforeSend"] != uint64(1) {
		t.Fatal("application limits or cancellation accounting failed", stats)
	}
}

func TestOutboundRateLimitIsSharedAcrossMessagesButNotEndpointGroups(t *testing.T) {
	var messages, cards atomic.Int32
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/messages/") {
			messages.Add(1)
			w.Header().Set("Retry-After", "10")
			jsonResponse(w, 429, core.Args{"code": 99991400})
			return
		}
		cards.Add(1)
		apiResponse(w, core.Args{})
	})
	for _, id := range []string{"om_first", "om_different"} {
		err := c.api(context.Background(), http.MethodPatch, "/im/v1/messages/"+id, nil, core.Args{"content":"fixture"}, nil)
		var failure core.DeliveryFailure
		if !errors.As(err, &failure) || failure.DeliveryUncertain() || !failure.DeliveryRetryable() || failure.DeliveryRetryAfter() < 8*time.Second {
			t.Fatal("shared 429 deadline was not preserved", err)
		}
	}
	if err := c.api(context.Background(), http.MethodPut, "/cardkit/v1/cards/card_final", nil, core.Args{}, nil); err != nil { t.Fatal(err) }
	if messages.Load() != 1 || cards.Load() != 1 {
		t.Fatal("rate-limited group still sent requests or unrelated group was blocked", messages.Load(), cards.Load())
	}
	data := c.Diagnostics()
	if data["http"].(core.Args)["rateLimited"] != uint64(1) { t.Fatal("missing content-free rate-limit diagnostic", data) }
}
