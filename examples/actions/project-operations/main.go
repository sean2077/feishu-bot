// A trusted host-side executor for the fixed resources in project-actions.json.
// The agent selects a configured operation and bounded parameters, never a URL,
// HTTP method, repository or target branch. Prefer existing trusted CLIs when available.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

var revisionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,99}$`)
var recordPattern = regexp.MustCompile(`^rec[A-Za-z0-9]+$`)

func required(names ...string) (map[string]string, error) {
	values := map[string]string{}
	for _, name := range names {
		v := os.Getenv(name)
		if v == "" {
			return nil, fmt.Errorf("missing setting: %s", name)
		}
		values[name] = v
	}
	return values, nil
}
func endpoint(name string) (string, error) {
	u, e := url.Parse(os.Getenv(name))
	if e != nil || u.Host == "" {
		return "", fmt.Errorf("invalid endpoint: %s", name)
	}
	local := os.Getenv("BOT_ACTION_TEST_HTTP") == "1" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")
	if (u.Scheme != "https" && !(u.Scheme == "http" && local)) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid endpoint: %s", name)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

type response struct {
	Data     map[string]any
	Location string
}

func request(ctx context.Context, method, target string, headers map[string]string, body any) (response, error) {
	var reader io.Reader
	if body != nil {
		data, e := json.Marshal(body)
		if e != nil {
			return response{}, errors.New("invalid operation JSON")
		}
		reader = bytes.NewReader(data)
	}
	req, e := http.NewRequestWithContext(ctx, method, target, reader)
	if e != nil {
		return response{}, errors.New("invalid operation request")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	r, e := client.Do(req)
	if e != nil {
		return response{}, errors.New("service request failed; write outcome may be unknown and is not retried")
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return response{}, fmt.Errorf("service returned HTTP %d; writes are not retried", r.StatusCode)
	}
	data, e := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if e != nil || len(data) > 1<<20 {
		return response{}, errors.New("service response failed or exceeded 1 MiB")
	}
	out := response{Data: map[string]any{}, Location: r.Header.Get("Location")}
	if len(bytes.TrimSpace(data)) > 0 {
		if json.Unmarshal(data, &out.Data) != nil {
			return response{}, errors.New("service response was not expected JSON")
		}
	}
	return out, nil
}
func jenkins(ctx context.Context, operation string, args []string) (any, error) {
	n := 0
	if operation == "jenkins-build" {
		n = 1
	}
	if len(args) != n {
		return nil, errors.New("invalid Jenkins arguments")
	}
	if n == 1 && !revisionPattern.MatchString(args[0]) {
		return nil, errors.New("invalid revision")
	}
	base, e := endpoint("JENKINS_URL")
	if e != nil {
		return nil, e
	}
	v, e := required("JENKINS_JOB", "JENKINS_USER", "JENKINS_TOKEN")
	if e != nil {
		return nil, e
	}
	job := ""
	for _, part := range strings.Split(v["JENKINS_JOB"], "/") {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("invalid configured job")
		}
		job += "/job/" + url.PathEscape(part)
	}
	headers := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(v["JENKINS_USER"]+":"+v["JENKINS_TOKEN"]))}
	if operation == "jenkins-status" {
		r, e := request(ctx, "GET", base+job+"/lastBuild/api/json?tree=number,result,building,url,duration,timestamp", headers, nil)
		return r.Data, e
	}
	r, e := request(ctx, "POST", base+job+"/buildWithParameters?"+url.Values{"REVISION": {args[0]}}.Encode(), headers, nil)
	if e != nil {
		return nil, e
	}
	return core.Args{"queued": true, "queueUrl": r.Location}, nil
}
func gitlabPublish(ctx context.Context, args []string) (any, error) {
	return gitlabPublishWith(ctx, args, func(cmd *exec.Cmd) error { return cmd.Run() })
}

// The runner is an internal test seam. Operation, repository, branch and every
// command argument are still constructed here from the configured fixed scope.
func gitlabPublishWith(ctx context.Context, args []string, execute func(*exec.Cmd) error) (any, error) {
	if len(args) != 2 || args[0] == "" || strings.TrimSpace(args[1]) == "" || len([]rune(args[1])) > 200 {
		return nil, errors.New("expected a snapshotted patch and short MR title")
	}
	patch, e := filepath.Abs(args[0])
	if e != nil {
		return nil, errors.New("invalid patch path")
	}
	st, e := os.Stat(patch)
	if e != nil || !st.Mode().IsRegular() {
		return nil, errors.New("snapshotted patch is unavailable")
	}
	base, e := endpoint("GITLAB_URL")
	if e != nil {
		return nil, e
	}
	repo, e := endpoint("GITLAB_REPO_URL")
	if e != nil {
		return nil, e
	}
	a, _ := url.Parse(base)
	b, _ := url.Parse(repo)
	if a.Scheme != b.Scheme || a.Host != b.Host {
		return nil, errors.New("repository and API must share an origin")
	}
	v, e := required("GITLAB_TOKEN", "GITLAB_BASE_BRANCH", "GITLAB_PROJECT_ID")
	if e != nil {
		return nil, e
	}
	if !revisionPattern.MatchString(v["GITLAB_BASE_BRANCH"]) {
		return nil, errors.New("invalid configured base branch")
	}
	branch := "feishu-bot/" + core.NewID()
	root, e := os.MkdirTemp("", "feishu-publish-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(root)
	work := filepath.Join(root, "checkout")
	hooks := filepath.Join(root, "empty-hooks")
	if e = os.Mkdir(hooks, 0700); e != nil {
		return nil, e
	}
	null := os.DevNull
	env := map[string]string{}
	for _, name := range []string{"PATH", "SystemRoot", "SYSTEMROOT", "COMSPEC", "PATHEXT", "TEMP", "TMP", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	env["HOME"] = root
	env["USERPROFILE"] = root
	env["GIT_CONFIG_NOSYSTEM"] = "1"
	env["GIT_CONFIG_GLOBAL"] = null
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_CONFIG_COUNT"] = "1"
	env["GIT_CONFIG_KEY_0"] = "http." + a.Scheme + "://" + a.Host + "/.extraHeader"
	env["GIT_CONFIG_VALUE_0"] = "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+v["GITLAB_TOKEN"]))
	git := os.Getenv("GIT_EXECUTABLE")
	if git == "" {
		git = "git"
	}
	run := func(argv ...string) error {
		options := []string{"-c", "core.hooksPath=" + hooks, "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "-c", "commit.gpgSign=false", "-c", "credential.helper=", "-c", "http.followRedirects=false"}
		cmd := exec.CommandContext(ctx, git, append(options, argv...)...)
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if err := execute(cmd); err != nil {
			return fmt.Errorf("git operation failed; inspect branch %s before retrying", branch)
		}
		return nil
	}
	for _, argv := range [][]string{{"clone", "--no-recurse-submodules", "--single-branch", "--depth", "1", "--branch", v["GITLAB_BASE_BRANCH"], "--", repo, work}, {"-C", work, "checkout", "-b", branch}, {"-C", work, "apply", "--check", "--", patch}, {"-C", work, "apply", "--index", "--", patch}, {"-C", work, "-c", "user.name=Feishu Project Bot", "-c", "user.email=feishu-bot@example.invalid", "commit", "-m", args[1]}, {"-C", work, "push", "origin", "HEAD:refs/heads/" + branch}} {
		if e = run(argv...); e != nil {
			return nil, e
		}
	}
	r, e := request(ctx, "POST", base+"/api/v4/projects/"+url.PathEscape(v["GITLAB_PROJECT_ID"])+"/merge_requests", map[string]string{"PRIVATE-TOKEN": v["GITLAB_TOKEN"]}, core.Args{"source_branch": branch, "target_branch": v["GITLAB_BASE_BRANCH"], "title": args[1], "remove_source_branch": true})
	if e != nil {
		return nil, fmt.Errorf("merge request creation unconfirmed for branch %s; inspect that existing branch before retrying: %w", branch, e)
	}
	webURL, _ := r.Data["web_url"].(string)
	u, parseErr := url.Parse(webURL)
	iid, validIID := r.Data["iid"].(float64)
	if parseErr != nil || u.Host != a.Host || u.Scheme != a.Scheme || u.User != nil || u.Path == "" || !validIID || iid < 1 || math.Trunc(iid) != iid {
		return nil, fmt.Errorf("merge request acknowledgement invalid for branch %s; inspect that existing branch before retrying", branch)
	}
	return core.Args{"url": webURL, "iid": iid, "branch": branch}, nil
}
func feishu(ctx context.Context, operation string, args []string) (any, error) {
	method, path := "GET", ""
	var body any
	switch operation {
	case "feishu-doc-read", "feishu-doc-append":
		v, e := required("FEISHU_DOCUMENT_ID")
		if e != nil {
			return nil, e
		}
		path = "/docx/v1/documents/" + url.PathEscape(v["FEISHU_DOCUMENT_ID"])
		if operation == "feishu-doc-read" {
			if len(args) != 0 {
				return nil, errors.New("unexpected document read arguments")
			}
			path += "/raw_content"
		} else {
			if len(args) != 1 || args[0] == "" || len([]rune(args[0])) > 10000 {
				return nil, errors.New("invalid document text")
			}
			root, e := required("FEISHU_DOCUMENT_ROOT_BLOCK")
			if e != nil {
				return nil, e
			}
			method = "POST"
			path += "/blocks/" + url.PathEscape(root["FEISHU_DOCUMENT_ROOT_BLOCK"]) + "/children"
			body = core.Args{"children": []core.Args{{"block_type": 2, "text": core.Args{"elements": []core.Args{{"text_run": core.Args{"content": args[0]}}}}}}}
		}
	case "feishu-table-list", "feishu-table-update":
		v, e := required("FEISHU_BASE_TOKEN", "FEISHU_TABLE_ID")
		if e != nil {
			return nil, e
		}
		path = "/bitable/v1/apps/" + url.PathEscape(v["FEISHU_BASE_TOKEN"]) + "/tables/" + url.PathEscape(v["FEISHU_TABLE_ID"]) + "/records"
		if operation == "feishu-table-list" {
			if len(args) != 0 {
				return nil, errors.New("unexpected table read arguments")
			}
			path += "?page_size=50"
		} else {
			if len(args) != 2 || !recordPattern.MatchString(args[0]) || len(args[1]) > 10000 {
				return nil, errors.New("invalid table update")
			}
			var fields map[string]any
			if json.Unmarshal([]byte(args[1]), &fields) != nil || len(fields) == 0 {
				return nil, errors.New("table fields must be a nonempty object")
			}
			v, e := required("FEISHU_WRITABLE_FIELDS")
			if e != nil {
				return nil, e
			}
			allowed := map[string]bool{}
			for _, name := range strings.Split(v["FEISHU_WRITABLE_FIELDS"], ",") {
				allowed[name] = true
			}
			for name := range fields {
				if !allowed[name] {
					return nil, errors.New("field is outside configured write scope")
				}
			}
			method = "PUT"
			path += "/" + url.PathEscape(args[0])
			body = core.Args{"fields": fields}
		}
	default:
		return nil, errors.New("unknown configured operation")
	}
	v, e := required("FEISHU_ACTION_APP_ID", "FEISHU_ACTION_APP_SECRET")
	if e != nil {
		return nil, e
	}
	base := "https://open.feishu.cn/open-apis"
	auth, e := request(ctx, "POST", base+"/auth/v3/tenant_access_token/internal", nil, core.Args{"app_id": v["FEISHU_ACTION_APP_ID"], "app_secret": v["FEISHU_ACTION_APP_SECRET"]})
	if e != nil {
		return nil, e
	}
	token, _ := auth.Data["tenant_access_token"].(string)
	if code, ok := auth.Data["code"].(float64); !ok || code != 0 || token == "" {
		return nil, errors.New("Feishu service identity authentication failed")
	}
	r, e := request(ctx, method, base+path, map[string]string{"Authorization": "Bearer " + token}, body)
	if e != nil {
		return nil, e
	}
	if code, ok := r.Data["code"].(float64); !ok || code != 0 {
		return nil, errors.New("Feishu operation rejected; inspect configured scope")
	}
	return r.Data["data"], nil
}
func run(ctx context.Context, args []string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("missing configured operation")
	}
	switch args[0] {
	case "jenkins-status", "jenkins-build":
		return jenkins(ctx, args[0], args[1:])
	case "gitlab-publish":
		return gitlabPublish(ctx, args[1:])
	default:
		return feishu(ctx, args[0], args[1:])
	}
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	value, err := run(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if json.NewEncoder(os.Stdout).Encode(value) != nil {
		os.Exit(1)
	}
}
