package native

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type actionRelay struct {
	url, token string
	server     *http.Server
	cancel     context.CancelFunc
}

func startActionRelay(ctx context.Context, actions []core.ActionSpec, invoke func(context.Context, string, args) args, requestContext func() *core.RequestContext) (*actionRelay, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		_ = listener.Close()
		return nil, err
	}
	relayCtx, cancel := context.WithCancel(ctx)
	relay := &actionRelay{url: "http://" + listener.Addr().String() + "/", token: hex.EncodeToString(token), cancel: cancel}
	names := map[string]bool{}
	for _, a := range actions {
		names[a.ID] = true
	}
	var active atomic.Int32
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 15 * time.Second, BaseContext: func(net.Listener) context.Context { return relayCtx }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(relay.token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/context" {
			if r.Method != "GET" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var current *core.RequestContext
			if requestContext != nil {
				current = requestContext()
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if current == nil {
				_ = json.NewEncoder(w).Encode(struct {
					Active bool `json:"active"`
				}{false})
				return
			}
			_ = json.NewEncoder(w).Encode(struct {
				Active bool `json:"active"`
				core.RequestContext
			}{true, *current})
			return
		}
		if r.Method != "POST" || r.URL.RequestURI() != "/" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if active.Add(1) > 8 {
			active.Add(-1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		defer active.Add(-1)
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				w.WriteHeader(413)
			} else {
				w.WriteHeader(400)
			}
			return
		}
		var data struct {
			Action     string         `json:"action"`
			Parameters map[string]any `json:"parameters"`
		}
		if json.Unmarshal(body, &data) != nil || !names[data.Action] || data.Parameters == nil {
			w.WriteHeader(400)
			return
		}
		result := invoke(r.Context(), data.Action, data.Parameters)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	})}
	relay.server = server
	go func() { _ = server.Serve(listener) }()
	return relay, nil
}
func (a *actionRelay) close() {
	if a != nil {
		a.cancel()
		_ = a.server.Close()
	}
}

// RunActionClient is used by `feishu-bot action <id> <json>` inside the native
// CLI's Bash or Monitor. It sends only to this conversation's loopback relay;
// the host's existing action policy is the authority for execution/approval.
func RunActionClient(ctx context.Context, argv []string, out io.Writer) error {
	if len(argv) < 1 || len(argv) > 2 || argv[0] == "" {
		return fmt.Errorf("usage: feishu-bot action <operation> '<parameters-json>'")
	}
	endpoint, token, err := actionRelayEndpoint()
	if err != nil {
		return err
	}
	raw := "{}"
	if len(argv) == 2 {
		raw = argv[1]
	}
	var parameters map[string]any
	if len(raw) > 65536 || json.Unmarshal([]byte(raw), &parameters) != nil || parameters == nil {
		return fmt.Errorf("parameters must be a JSON object")
	}
	body, err := json.Marshal(args{"action": argv[0], "parameters": parameters})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("action bridge redirect rejected") }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("action bridge unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("action bridge returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return fmt.Errorf("invalid action bridge response")
	}
	var result args
	if json.Unmarshal(data, &result) != nil {
		return fmt.Errorf("invalid action bridge response")
	}
	if err = json.NewEncoder(out).Encode(result); err != nil {
		return err
	}
	if !truth(result["ok"]) {
		return fmt.Errorf("configured operation did not succeed")
	}
	return nil
}

// actionRelayEndpoint validates the loopback relay environment shared by the
// action and context clients.
func actionRelayEndpoint() (*url.URL, string, error) {
	endpoint, token := os.Getenv("FEISHU_ACTION_URL"), os.Getenv("FEISHU_ACTION_TOKEN")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || token == "" {
		return nil, "", fmt.Errorf("this command must run inside a feishu-bot conversation")
	}
	return u, token, nil
}

// RunContextClient prints the request context visible in the current native
// session. It is read-only and only reaches this conversation's loopback relay.
func RunContextClient(ctx context.Context, out io.Writer) error {
	base, token, err := actionRelayEndpoint()
	if err != nil {
		return err
	}
	endpoint := *base
	endpoint.Path = "/context"
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("action bridge redirect rejected") }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request context unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("request context unavailable (HTTP %d)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return fmt.Errorf("invalid context response")
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || value == nil {
		return fmt.Errorf("invalid context response")
	}
	return json.NewEncoder(out).Encode(value)
}
