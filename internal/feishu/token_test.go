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
)

type tokenResult struct { token string; err error }

func awaitToken(t *testing.T, result <-chan tokenResult) tokenResult {
	t.Helper()
	select {
	case value := <-result: return value
	case <-time.After(3*time.Second): t.Fatal("token refresh did not finish"); return tokenResult{}
	}
}

func successfulToken() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))}
}

func TestTokenRefreshWaiterCancelsWithoutCancellingTheOwner(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("unexpected fixture HTTP") })
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	c.http.Transport = deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 { close(started) }
		select { case <-release: return successfulToken(), nil; case <-req.Context().Done(): return nil, req.Context().Err() }
	})
	owner := make(chan tokenResult, 1)
	go func() { token, err := c.accessToken(context.Background()); owner <- tokenResult{token, err} }()
	await(t, started, "owner token request")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := make(chan tokenResult, 1)
	ready := make(chan struct{})
	go func() { close(ready); token, err := c.accessToken(ctx); waiter <- tokenResult{token, err} }()
	await(t, ready, "refresh waiter")
	cancel()
	if got := awaitToken(t, waiter); !errors.Is(got.err, context.Canceled) || got.token != "" { t.Fatal("cancelled waiter ignored its deadline", got.err) }
	select { case <-owner: t.Fatal("cancelled waiter aborted another caller's refresh"); default: }
	if calls.Load() != 1 || c.budget.snapshot()["active"] != 1 { t.Fatal("waiter duplicated or cancelled the active HTTP refresh", calls.Load(), c.Diagnostics()) }
	unblock()
	if got := awaitToken(t, owner); got.err != nil || got.token != "fixture-token" { t.Fatal("owner could not finish refresh", got.err) }
	if token, err := c.accessToken(context.Background()); err != nil || token != "fixture-token" || calls.Load() != 1 { t.Fatal("completed refresh was not cached", err) }
}

func TestConcurrentTokenCallersShareOneSuccessfulRefresh(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("unexpected fixture HTTP") })
	const callers = 32
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	c.http.Transport = deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 { close(started) }
		select { case <-release: return successfulToken(), nil; case <-req.Context().Done(): return nil, req.Context().Err() }
	})
	results := make(chan tokenResult, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for i := 0; i < callers; i++ {
		go func() { ready.Done(); token, err := c.accessToken(context.Background()); results <- tokenResult{token, err} }()
	}
	ready.Wait()
	await(t, started, "shared token request")
	unblock()
	for i := 0; i < callers; i++ {
		if got := awaitToken(t, results); got.err != nil || got.token != "fixture-token" { t.Fatal("shared refresh failed", got.err) }
	}
	if calls.Load() != 1 || c.budget.snapshot()["active"] != 0 { t.Fatal("concurrent callers performed duplicate refreshes", calls.Load(), c.Diagnostics()) }
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.tokenFlight != nil { t.Fatal("completed refresh retained its single-flight state") }
}

func TestCancelledTokenOwnerDoesNotPoisonAnotherCallersContext(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("unexpected fixture HTTP") })
	started := make(chan struct{})
	var calls atomic.Int32
	c.http.Transport = deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return successfulToken(), nil
	})
	ownerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := make(chan tokenResult, 1)
	go func() { token, err := c.accessToken(ownerCtx); owner <- tokenResult{token, err} }()
	await(t, started, "cancelled owner request")
	waiter := make(chan tokenResult, 1)
	go func() { token, err := c.accessToken(context.Background()); waiter <- tokenResult{token, err} }()
	cancel()
	if got := awaitToken(t, owner); !errors.Is(got.err, context.Canceled) { t.Fatal("owner cancellation lost", got.err) }
	if got := awaitToken(t, waiter); got.err != nil || got.token != "fixture-token" || calls.Load() != 2 { t.Fatal("a different caller inherited the cancelled owner's context", got.err, calls.Load()) }
}

func TestOldUnauthorizedResponseCannotInvalidateNewToken(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(http.ResponseWriter, *http.Request) { t.Error("unexpected fixture HTTP") })
	c.token, c.tokenUntil = "new-token", time.Now().Add(time.Hour)
	c.http.Transport = deliveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":99991663}`))}, nil
	})
	req := budgetRequest(context.Background(), "im/v1/messages/om_old")
	req.Header.Set("Authorization", "Bearer old-token")
	if err := c.doAPI(req, nil, true); err == nil { t.Fatal("old unauthorized response was accepted") }
	if token, err := c.accessToken(context.Background()); err != nil || token != "new-token" { t.Fatal("old response erased the current credential", err) }
}
