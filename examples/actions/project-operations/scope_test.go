package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestAcceptedWriteWithLostAcknowledgementIsNotRetried(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writes.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	_, err := request(context.Background(), http.MethodPost, server.URL, map[string]string{"Authorization": "fixture-secret"}, core.Args{"revision": "main"})
	if err == nil || writes.Load() != 1 || !strings.Contains(err.Error(), "unknown") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("ambiguous write was mishandled: writes=%d err=%v", writes.Load(), err)
	}
}

func TestGitLabPublishRetainsBranchOnUnknownOrInvalidMRReceipt(t *testing.T) {
	for _, scenario := range []string{"lost-response", "missing-receipt", "success"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int32
			var requested atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects/123/merge_requests" || r.Header.Get("PRIVATE-TOKEN") != "fixture-gitlab-token" {
					t.Errorf("request escaped fixed project: %s %s", r.Method, r.URL.Path)
				}
				var body core.Args
				if json.NewDecoder(r.Body).Decode(&body) != nil || body["target_branch"] != "main" || body["title"] != "Approved patch" {
					t.Errorf("invalid MR payload: %#v", body)
				}
				branch, _ := body["source_branch"].(string)
				requested.Store(branch)
				switch scenario {
				case "lost-response":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "missing-receipt":
					_, _ = io.WriteString(w, `{}`)
				default:
					_ = json.NewEncoder(w).Encode(core.Args{"web_url": "http://" + r.Host + "/team/project/-/merge_requests/7", "iid": 7})
				}
			}))
			defer server.Close()
			t.Setenv("BOT_ACTION_TEST_HTTP", "1")
			t.Setenv("GITLAB_URL", server.URL)
			t.Setenv("GITLAB_REPO_URL", server.URL+"/team/project.git")
			t.Setenv("GITLAB_TOKEN", "fixture-gitlab-token")
			t.Setenv("GITLAB_PROJECT_ID", "123")
			t.Setenv("GITLAB_BASE_BRANCH", "main")
			t.Setenv("GIT_CONFIG_GLOBAL", "/untrusted-global-config")
			t.Setenv("GIT_TEMPLATE_DIR", "/untrusted-template")
			t.Setenv("SSH_AUTH_SOCK", "/unrelated-credential")
			patch := filepath.Join(t.TempDir(), "approved.patch")
			if err := os.WriteFile(patch, []byte("already snapshotted by the gateway"), 0600); err != nil {
				t.Fatal(err)
			}
			var commands [][]string
			value, err := gitlabPublishWith(context.Background(), []string{patch, "Approved patch"}, func(cmd *exec.Cmd) error {
				commands = append(commands, append([]string(nil), cmd.Args...))
				for _, value := range cmd.Env {
					if strings.HasPrefix(value, "GIT_TEMPLATE_DIR=") || strings.HasPrefix(value, "SSH_AUTH_SOCK=") || value == "GIT_CONFIG_GLOBAL=/untrusted-global-config" {
						t.Errorf("inherited unrelated process authority: %s", value)
					}
				}
				joined := strings.Join(cmd.Args, "\n")
				for _, setting := range []string{"core.hooksPath=", "protocol.file.allow=never", "protocol.ext.allow=never", "http.followRedirects=false", "credential.helper="} {
					if !strings.Contains(joined, setting) {
						t.Errorf("missing fixed git setting %s", setting)
					}
				}
				return nil
			})
			requestedBranch, _ := requested.Load().(string)
			if len(commands) != 6 || requests.Load() != 1 || !strings.HasPrefix(requestedBranch, "feishu-bot/") {
				t.Fatalf("unexpected command/request count: commands=%d requests=%d branch=%q", len(commands), requests.Load(), requestedBranch)
			}
			push := commands[len(commands)-1]
			if len(push) < 3 || strings.Join(push[len(push)-3:], " ") != "push origin HEAD:refs/heads/"+requestedBranch {
				t.Fatalf("push escaped fixed new branch: %v", push)
			}
			if scenario == "success" {
				if err != nil || value.(core.Args)["branch"] != requestedBranch {
					t.Fatalf("successful publication failed: %v %v", value, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), requestedBranch) || !strings.Contains(err.Error(), "before retrying") || strings.Contains(err.Error(), "fixture-gitlab-token") {
				t.Fatalf("lost recovery information after pushed branch: %v", err)
			}
		})
	}
}

type fixtureRoundTripper func(*http.Request) (*http.Response, error)

func (f fixtureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeishuExecutorBindsDocumentTableAndWritableFields(t *testing.T) {
	t.Setenv("FEISHU_ACTION_APP_ID", "cli_fixture")
	t.Setenv("FEISHU_ACTION_APP_SECRET", "fixture-action-secret")
	t.Setenv("FEISHU_DOCUMENT_ID", "doc_configured")
	t.Setenv("FEISHU_DOCUMENT_ROOT_BLOCK", "block_configured")
	t.Setenv("FEISHU_BASE_TOKEN", "base_configured")
	t.Setenv("FEISHU_TABLE_ID", "tbl_configured")
	t.Setenv("FEISHU_WRITABLE_FIELDS", "Status,Notes")
	before := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = before })
	var paths []string
	http.DefaultTransport = fixtureRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "open.feishu.cn" {
			t.Fatalf("unexpected service origin: %s", r.URL)
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		body := `{"code":0,"data":{"ok":true}}`
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			body = `{"code":0,"tenant_access_token":"fixture-action-token"}`
		} else {
			if r.Header.Get("Authorization") != "Bearer fixture-action-token" {
				t.Fatal("operation did not use dedicated service identity")
			}
			if r.Method == http.MethodPut {
				var value core.Args
				if json.NewDecoder(r.Body).Decode(&value) != nil || value["fields"].(map[string]any)["Status"] != "Ready" {
					t.Fatal("wrong table mutation")
				}
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	for _, tc := range []struct {
		operation string
		args      []string
		path      string
	}{
		{"feishu-doc-read", nil, "GET /open-apis/docx/v1/documents/doc_configured/raw_content"},
		{"feishu-doc-append", []string{"approved text"}, "POST /open-apis/docx/v1/documents/doc_configured/blocks/block_configured/children"},
		{"feishu-table-update", []string{"rec123", `{"Status":"Ready"}`}, "PUT /open-apis/bitable/v1/apps/base_configured/tables/tbl_configured/records/rec123"},
	} {
		if _, err := feishu(context.Background(), tc.operation, tc.args); err != nil {
			t.Fatal(err)
		}
		if paths[len(paths)-1] != tc.path {
			t.Fatalf("wrong resource: %s", paths[len(paths)-1])
		}
	}
	count := len(paths)
	for _, args := range [][]string{
		{"rec123", `{"Other":"outside scope"}`},
		{"rec123/../../other", `{"Status":"Ready"}`},
		{"rec123", `{}`},
		{"rec123", `null`},
		{"rec123", `[]`},
	} {
		if _, err := feishu(context.Background(), "feishu-table-update", args); err == nil {
			t.Fatalf("accepted out-of-scope update: %v", args)
		}
	}
	if len(paths) != count {
		t.Fatal("invalid mutation made a network request")
	}
}
