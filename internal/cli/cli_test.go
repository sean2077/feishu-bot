package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestCLIRejectsAmbiguousOptions(t *testing.T) {
	for _, args := range [][]string{{"run", "--probe"}, {"init", "--probe-feishu"}, {"doctor", "--probe", "--probe"}, {"run", "--config"}, {"run", "--config", "one", "--config=two"}, {"doctor", "--unknown"}, {"version", "extra"}, {"wat"}} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	o, err := Parse([]string{"doctor", "--config=relative.json", "--probe", "--probe-feishu"})
	if err != nil || !filepath.IsAbs(o.ConfigPath) || !o.Probe || !o.ProbeFeishu {
		t.Fatal(o, err)
	}
}
func TestInitPreservesExistingConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	var out bytes.Buffer
	if err := initialize(path, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing deployment"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := initialize(path, &out); err == nil {
		t.Fatal("overwrote existing configuration")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "existing deployment" {
		t.Fatal("configuration changed")
	}
}
func TestNoConfigNeededForHelpOrVersion(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"version"}} {
		var out, errs bytes.Buffer
		if code := Run(context.Background(), args, &out, &errs); code != 0 || out.Len() == 0 || errs.Len() != 0 {
			t.Fatal(code, out.String(), errs.String())
		}
	}
}
func TestInvalidJSONDoesNotExposeSource(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(p, []byte(`{"secret":"do-not-print-secret", oops}`), 0600)
	var out, errs bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--config", p}, &out, &errs); code == 0 || strings.Contains(errs.String(), "do-not-print-secret") {
		t.Fatal(code, errs.String())
	}
}
func TestDoctorOfflineDoesNotOpenProductionDatabase(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.Model.BaseURL = "https://model.example"
	c.Runtime.ClaudeExecutable = filepath.Join(c.DataDir, "missing-claude")
	state := filepath.Join(c.DataDir, "gateway.sqlite")
	_ = os.WriteFile(state, []byte("deployment database must not be touched"), 0600)
	report := RunDoctor(context.Background(), c, false, false)
	data, _ := os.ReadFile(state)
	if string(data) != "deployment database must not be touched" {
		t.Fatal("doctor changed production state")
	}
	for _, key := range []string{"model", "feishuAuth", "feishuIdentity", "messageDelivery", "cardCallbacks", "fileDelivery", "nativeTools"} {
		if report.Verification[key] != "not_checked" {
			t.Fatalf("claimed %s: %s", key, report.Verification[key])
		}
	}
	entries, _ := os.ReadDir(c.DataDir)
	if len(entries) != 1 {
		t.Fatal("temporary doctor state was retained")
	}
}
func TestModelProbeUsesAPIModelAndDoesNotLeakResponse(t *testing.T) {
	t.Setenv("CLI_TEST_TOKEN", "test-token")
	var requestModel string
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		requestModel = b.Model
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"OK"}]}`)
	}))
	defer server.Close()
	c := config.Defaults()
	c.Model.BaseURL = server.URL
	c.Model.Model = "model-x[1m]"
	c.Model.TokenEnv = "CLI_TEST_TOKEN"
	if err := probeModel(context.Background(), server.Client(), c); err != nil {
		t.Fatal(err)
	}
	if requestModel != "model-x" || auth != "Bearer test-token" || c.Model.Model != "model-x[1m]" {
		t.Fatal("probe changed native model or credentials")
	}
	resp := &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("secret-provider-error"))}
	err := decodeProbe(resp, &map[string]any{})
	if err == nil || strings.Contains(err.Error(), "secret-provider-error") {
		t.Fatal("provider diagnostics leaked")
	}
}
func TestFeishuIdentityProbeHasDistinctStages(t *testing.T) {
	botCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"ephemeral"}`)
		} else {
			botCalls++
			if r.Header.Get("Authorization") != "Bearer ephemeral" {
				t.Error("missing tenant token")
			}
			_, _ = io.WriteString(w, `{"code":0,"bot":{"open_id":"bot-identity"}}`)
		}
	}))
	defer server.Close()
	a, b, err := probeIdentity(context.Background(), server.Client(), "app", "secret", server.URL)
	if err != nil || a != "passed" || b != "passed" || botCalls != 1 {
		t.Fatal(a, b, err, botCalls)
	}
}

type fakeGateway struct {
	st      *store.Store
	recover func(context.Context) error
	stopErr error
	stop    func(context.Context) error
	closed  bool
}

func (g *fakeGateway) Recover(ctx context.Context) error {
	if g.recover != nil {
		return g.recover(ctx)
	}
	return nil
}
func (g *fakeGateway) Message(context.Context, core.Message) error             { return nil }
func (g *fakeGateway) Action(context.Context, core.CardAction) (string, error) { return "", nil }
func (g *fakeGateway) Close(ctx context.Context) error {
	g.closed = true
	if !g.st.IsOpen() {
		return errors.New("database closed before gateway")
	}
	if g.stop != nil {
		return g.stop(ctx)
	}
	return g.stopErr
}

type fakeChannel struct {
	started, closed bool
	start           func(context.Context) error
}

func (c *fakeChannel) Start(ctx context.Context, _ func(context.Context, core.Message) error, _ func(context.Context, core.CardAction) (string, error)) error {
	c.started = true
	if c.start != nil {
		return c.start(ctx)
	}
	return nil
}
func (c *fakeChannel) Close() error { c.closed = true; return nil }
func TestShutdownDuringRecoveryDoesNotOpenConnection(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &fakeGateway{}
	ch := &fakeChannel{}
	g.recover = func(ctx context.Context) error {
		cancel()
		if !g.st.IsOpen() {
			t.Fatal("state closed during recovery")
		}
		return ctx.Err()
	}
	err := runService(ctx, c, func(string, core.Args) {}, func(_ core.Config, st *store.Store, _ core.Logger) (serviceGateway, serviceChannel, error) {
		g.st = st
		return g, ch, nil
	})
	if err != nil || ch.started || !ch.closed || !g.closed || g.st.IsOpen() {
		t.Fatal(err, ch, g.closed)
	}
	if _, err = os.Stat(filepath.Join(c.DataDir, "gateway.lock")); !os.IsNotExist(err) {
		t.Fatal("confirmed shutdown left the lock")
	}
}
func TestUnconfirmedShutdownKeepsExclusiveLock(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	g := &fakeGateway{stopErr: errors.New("termination unconfirmed")}
	ch := &fakeChannel{}
	err := runService(context.Background(), c, func(string, core.Args) {}, func(_ core.Config, st *store.Store, _ core.Logger) (serviceGateway, serviceChannel, error) {
		g.st = st
		return g, ch, nil
	})
	if err == nil || !ch.closed || g.st.IsOpen() {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(c.DataDir, "gateway.lock")); err != nil {
		t.Fatal("unconfirmed process stop released the lock", err)
	}
}

func TestShutdownKeepsTransportAndDatabaseAvailableForFinalCards(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := &fakeChannel{start: func(ctx context.Context) error { cancel(); return ctx.Err() }}
	g := &fakeGateway{}
	g.stop = func(ctx context.Context) error {
		if ctx.Err() != nil {
			return errors.New("shutdown inherited the cancelled connection context")
		}
		if ch.closed {
			return errors.New("transport closed before final approval-card updates")
		}
		if _, err := g.st.Session("shutdown-fixture"); err != nil {
			return err
		}
		return nil
	}
	err := runService(parent, c, func(string, core.Args) {}, func(_ core.Config, st *store.Store, _ core.Logger) (serviceGateway, serviceChannel, error) {
		g.st = st
		return g, ch, nil
	})
	if err != nil || !g.closed || !ch.closed || g.st.IsOpen() {
		t.Fatal("incomplete ordered shutdown", err)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "gateway.lock")); !os.IsNotExist(err) {
		t.Fatal("confirmed shutdown retained lock", err)
	}
}

func TestCancelledInitAndDoctorDoNotCreateFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "new", "config.json")
	var out, errs bytes.Buffer
	if code := Run(ctx, []string{"init", "--config", path}, &out, &errs); code == 0 {
		t.Fatal("cancelled init reported success")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("cancelled init created a directory", err)
	}
	c := config.Defaults()
	c.DataDir = filepath.Join(t.TempDir(), "doctor-state")
	report := RunDoctor(ctx, c, false, false)
	if report.Ready {
		t.Fatal("cancelled doctor claimed readiness")
	}
	if _, err := os.Stat(c.DataDir); !os.IsNotExist(err) {
		t.Fatal("cancelled doctor created probe state", err)
	}
	out.Reset()
	errs.Reset()
	if code := Run(ctx, []string{"run", "--config", path}, &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatal("cancelled run attempted startup", code, errs.String())
	}
}
