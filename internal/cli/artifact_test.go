package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
)

// This launches the actual CGO-disabled artifact supplied by CI, not a test
// binary or cli.Run. Fake Feishu is reached only through a local deny-by-default
// CONNECT fixture. No deployment credential or real provider is involved.
func TestBuiltArtifactIdentityInitAndDiagnosticJourney(t *testing.T) {
	identity := readArtifactIdentity(t, "FEISHU_ARTIFACT")
	binary, expected := identity.Path, identity.Version
	claude := os.Getenv("CLAUDE_NATIVE_PATH")
	if claude == "" {
		t.Fatal("artifact journey requires the real pinned native CLI")
	}
	dir := t.TempDir()
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer artifact-model-fixture" {
			http.Error(w, "unexpected fixture request", 400)
			return
		}
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"OK"}]}`)
	}))
	defer model.Close()
	env := []string{"ARTIFACT_APP_SECRET=artifact-app-fixture", "ARTIFACT_MODEL_TOKEN=artifact-model-fixture"}
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.Command(binary, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := process.Run(ctx, cmd)
		if err != nil {
			t.Logf("artifact stderr: %s", stderr.String())
		}
		return stdout.Bytes(), err
	}
	version, err := run("--version")
	if err != nil || strings.TrimSpace(string(version)) != "feishu-bot "+expected {
		t.Fatal("wrong built artifact identity", string(version), err)
	}
	path := filepath.Join(dir, "config.json")
	if _, err := run("init", "--config", path); err != nil {
		t.Fatal("artifact init", err)
	}
	if _, err := run("init", "--config", path); err == nil {
		t.Fatal("artifact overwrote existing configuration")
	}
	c := config.Defaults()
	c.TenantID, c.AppID = "tenant_fixture", "cli_fixture"
	c.AppSecretEnv, c.Model.TokenEnv = "ARTIFACT_APP_SECRET", "ARTIFACT_MODEL_TOKEN"
	c.DataDir = filepath.Join(dir, "state")
	c.Runtime.ClaudeExecutable = claude
	c.Model.BaseURL, c.Model.AllowHTTP = model.URL, true
	c.Model.Model, c.Model.SmallModel = "fixture-model", "fixture-model"
	c.Projects = []core.Project{{ID: "fixture", Maintainers: []string{"ou_owner"}, PermissionMode: "acceptEdits"}}
	c.Groups = []core.Group{{ChatID: "oc_fixture", ProjectID: "fixture", Members: []string{"ou_member"}}}
	c.DMs = nil
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	data, err = run("doctor", "--config", path)
	var local DoctorReport
	if err != nil || json.Unmarshal(data, &local) != nil || !local.Ready || local.Native.Actual != native.TestedVersion || local.Verification["nativeProtocol"] != "not_checked" || modelCalls.Load() != 0 {
		t.Fatalf("artifact local diagnostic contract: err=%v report=%s", err, data)
	}
	data, err = run("doctor", "--config", path, "--probe")
	var report DoctorReport
	if err != nil || json.Unmarshal(data, &report) != nil || report.Verification["model"] != "passed" || modelCalls.Load() != 1 || report.Verification["modelStreaming"] != "not_checked" {
		t.Fatalf("artifact local-provider diagnostic contract: err=%v report=%s", err, data)
	}
	proxy, certificate, calls := artifactFeishuProxy(t)
	defer proxy.Close()
	env = append(env, artifactTrustEnvironment(t, certificate, dir)...)
	// These variables belong only to the child fixture process. The proxy never
	// forwards to the requested public host; its sole destination is loopback.
	env = append(env, "HTTPS_PROXY="+proxy.URL, "HTTP_PROXY="+proxy.URL, "ALL_PROXY="+proxy.URL, "NO_PROXY=127.0.0.1,localhost", "https_proxy="+proxy.URL, "http_proxy="+proxy.URL, "all_proxy="+proxy.URL, "no_proxy=127.0.0.1,localhost")
	data, err = run("doctor", "--config", path, "--probe-feishu")
	if err != nil || json.Unmarshal(data, &report) != nil || report.Verification["feishuAuth"] != "passed" || report.Verification["feishuIdentity"] != "passed" || calls.Load() != 2 || report.Verification["messageDelivery"] != "not_checked" {
		t.Fatalf("artifact fake-Feishu identity contract: err=%v report=%s calls=%d", err, data, calls.Load())
	}
}

func artifactFeishuProxy(t *testing.T, custom ...http.Handler) (*httptest.Server, []byte, *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "feishu-artifact-fixture-" + serial.Text(16)}, DNSNames: []string{"open.feishu.cn"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(cert, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			calls.Add(1)
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"local-token","expire":7200}`)
		case "/open-apis/bot/v3/info":
			if r.Header.Get("Authorization") != "Bearer local-token" {
				http.Error(w, "fixture authorization", 403)
				return
			}
			calls.Add(1)
			_, _ = io.WriteString(w, `{"code":0,"bot":{"open_id":"ou_fixture_bot"},"data":{"open_id":"ou_fixture_bot"}}`)
		default:
			http.Error(w, "fixture endpoint only", 404)
		}
	}))
	if len(custom) > 0 {
		handler = custom[0]
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	var tunnelMu sync.Mutex
	tunnels := map[net.Conn]bool{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "open.feishu.cn:443" {
			http.Error(w, "fixture destination denied", 403)
			return
		}
		upstream, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
		if err != nil {
			http.Error(w, "fixture unavailable", 502)
			return
		}
		downstream, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		defer upstream.Close()
		defer downstream.Close()
		tunnelMu.Lock()
		tunnels[upstream], tunnels[downstream] = true, true
		tunnelMu.Unlock()
		defer func() { tunnelMu.Lock(); delete(tunnels, upstream); delete(tunnels, downstream); tunnelMu.Unlock() }()
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffer.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffer); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(downstream, upstream)
		_ = downstream.Close()
		<-done
	}))
	t.Cleanup(func() {
		tunnelMu.Lock()
		active := make([]net.Conn, 0, len(tunnels))
		for conn := range tunnels {
			active = append(active, conn)
		}
		tunnelMu.Unlock()
		for _, conn := range active {
			_ = conn.Close()
		}
		proxy.Close()
	})
	return proxy, cert, &calls
}
