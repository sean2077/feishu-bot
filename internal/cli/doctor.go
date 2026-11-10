package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
)

type Diagnostic struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
	Remedy  string `json:"remedy,omitempty"`
}
type DoctorReport struct {
	Platform     string                      `json:"platform"`
	Runtime      string                      `json:"runtime"`
	Ready        bool                        `json:"ready"`
	Scope        string                      `json:"scope"`
	Checks       []Diagnostic                `json:"checks"`
	Verification map[string]string           `json:"verification"`
	NextSteps    []string                    `json:"nextSteps"`
	Native       native.VersionCompatibility `json:"native"`
	Storage      StorageDiagnostic           `json:"storage"`
	LiveChat     *ChatVerification           `json:"liveChat,omitempty"`
}

var placeholderPattern = regexp.MustCompile(`(?i)^(your[-_ ]|replace[-_ ]with[-_ ]|<[^>]+>$|(tenant|cli|oc|ou)_example(_|$))`)

func placeholder(s string) bool { return placeholderPattern.MatchString(strings.TrimSpace(s)) }
func deploymentChecks(c core.Config) []Diagnostic {
	unfinished := []string{}
	check := func(name, s string) {
		if placeholder(s) {
			unfinished = append(unfinished, name)
		}
	}
	check("tenantId", c.TenantID)
	check("appId", c.AppID)
	check("model.model", c.Model.Model)
	check("model.smallModel", c.Model.SmallModel)
	u, _ := url.Parse(c.Model.BaseURL)
	host := ""
	if u != nil {
		host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	for _, domain := range []string{"example", "invalid", "example.com", "example.net", "example.org"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			unfinished = append(unfinished, "model.baseUrl")
			break
		}
	}
	for i, p := range c.Projects {
		for j, id := range p.Maintainers {
			check(fmt.Sprintf("projects[%d].maintainers[%d]", i, j), id)
		}
		for _, action := range p.Actions {
			for j, id := range action.Approvers {
				check(fmt.Sprintf("projects[%d].actions[%s].approvers[%d]", i, action.ID, j), id)
			}
		}
	}
	for i, g := range c.Groups {
		check(fmt.Sprintf("groups[%d].chatId", i), g.ChatID)
		for j, id := range g.Members {
			check(fmt.Sprintf("groups[%d].members[%d]", i, j), id)
		}
	}
	for i, d := range c.DMs {
		check(fmt.Sprintf("dms[%d].userId", i), d.UserID)
	}
	checks := []Diagnostic{{Name: "configuration values", OK: len(unfinished) == 0, Detail: "Replace template fields: " + strings.Join(unfinished, ", ")}, {Name: "chat bindings", OK: len(c.Groups)+len(c.DMs) > 0, Detail: "At least one authorized group or DM user is required."}}
	names := []string{c.AppSecretEnv, c.Model.TokenEnv}
	seen := map[string]bool{}
	for _, p := range c.Projects {
		for _, a := range p.Actions {
			for _, name := range a.Env {
				names = append(names, name)
			}
		}
	}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		value := os.Getenv(name)
		ok := strings.TrimSpace(value) != "" && !placeholder(value)
		checks = append(checks, Diagnostic{Name: "credential " + name, OK: ok, Detail: "Credential presence only; value is not displayed."})
	}
	h, err := config.ModelHeaders(c)
	if err == nil {
		for _, v := range h {
			if placeholder(v) {
				err = errors.New("replace template model headers")
				break
			}
		}
	}
	checks = append(checks, Diagnostic{Name: "model endpoint headers", OK: err == nil})
	return checks
}

var secretEnvName = regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|AUTHORIZATION|CREDENTIALS?|PRIVATE_KEY)($|_)`)

// projectEnvChecks are reported by doctor and its installation precheck, but
// are deliberately separate from deploymentChecks so a missing passthrough
// does not block foreground `run`. A declared passthrough name is still a
// deployment requirement for daemon install and upgrades: the installation
// precheck uses the complete doctor report before changing an existing
// installation.
func projectEnvChecks(c core.Config) []Diagnostic {
	checked := map[string]bool{}
	var checks []Diagnostic
	for _, p := range c.Projects {
		for _, name := range p.EnvPassthrough {
			if checked[name] {
				continue
			}
			checked[name] = true
			value := os.Getenv(name)
			ok := strings.TrimSpace(value) != "" && !placeholder(value)
			checks = append(checks, Diagnostic{Name: "project " + p.ID + " credential " + name, OK: ok, Detail: "Declared project passthrough variable; presence only, value is not displayed."})
		}
		names := make([]string, 0, len(p.Env))
		for name := range p.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if secretEnvName.MatchString(name) {
				checks = append(checks, Diagnostic{Name: "project " + p.ID + " env " + name, OK: false, Detail: "Variable name looks like a credential; use envPassthrough for secrets instead of a literal env value."})
			}
		}
	}
	return checks
}

func projectSettingsChecks(c core.Config) []Diagnostic {
	var checks []Diagnostic
	for _, p := range c.Projects {
		if p.SettingsFile == "" {
			continue
		}
		_, err := config.LoadProjectSettings(c, p)
		detail := "Settings file is readable and structurally valid; rule contents are not displayed."
		if err != nil {
			detail = core.Redact(err.Error(), config.Secrets(c))
		}
		checks = append(checks, Diagnostic{Name: "project " + p.ID + " settings file", OK: err == nil, Detail: detail})
	}
	return checks
}

func approvalPolicyChecks(c core.Config) []Diagnostic {
	var checks []Diagnostic
	for _, p := range c.Projects {
		if p.Approvals.ForbidSelfApproval && len(p.Owners) == 0 {
			checks = append(checks, Diagnostic{Name: "project " + p.ID + " approval policy", OK: true, Detail: "Warning: forbidSelfApproval is enabled without owners; a maintainer cannot approve their own requests, and a sole maintainer has no other approver."})
		}
	}
	return checks
}

func permissionModeChecks(c core.Config) []Diagnostic {
	var checks []Diagnostic
	for _, p := range c.Projects {
		mode := p.PermissionMode
		if mode == "" {
			mode = "acceptEdits"
		}
		detail := "Configured permissionMode: " + mode + "."
		if mode == "auto" {
			detail += " Auto mode forces the CLI's local safety classifier (CLAUDE_CODE_AUTO_MODE_SERVER=0); nonessential traffic stays disabled. The classifier uses claude-sonnet-5 through model.baseUrl, sends up to two billable requests per classification, and its availability is not_checked by doctor."
		}
		checks = append(checks, Diagnostic{Name: "project " + p.ID + " permission mode", OK: true, Detail: detail})
	}
	return checks
}

// feedbackExportChecks validates the configured archive directory before the
// service relies on it to precede feedback retention. It only creates and
// removes a temporary probe file; archive contents are never read.
func feedbackExportChecks(c core.Config) []Diagnostic {
	if !c.Feedback.Export.Enabled {
		return nil
	}
	const name = "feedback export directory"
	dir := c.Feedback.Export.Dir
	info, err := os.Lstat(dir)
	if err != nil {
		return []Diagnostic{{Name: name, OK: false, Detail: "Configured feedback export directory is missing or unreadable."}}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return []Diagnostic{{Name: name, OK: false, Detail: "Configured feedback export directory must not be a symbolic link."}}
	}
	if !info.IsDir() {
		return []Diagnostic{{Name: name, OK: false, Detail: "Configured feedback export path is not a directory."}}
	}
	probe, err := os.CreateTemp(dir, ".doctor-feedback-*")
	if err != nil {
		return []Diagnostic{{Name: name, OK: false, Detail: "Configured feedback export directory is not writable."}}
	}
	probePath := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probePath)
	if closeErr != nil || removeErr != nil {
		return []Diagnostic{{Name: name, OK: false, Detail: "Configured feedback export directory failed its write probe."}}
	}
	return []Diagnostic{{Name: name, OK: true, Detail: "Existing writable directory; archive contents are not inspected."}}
}

type cappedWriter struct {
	bytes.Buffer
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.Len() < w.limit {
		remaining := w.limit - w.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		w.Buffer.Write(p)
	}
	return n, nil
}
func versionCommand(ctx context.Context, program string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.Command(program, args...)
	b := &cappedWriter{limit: 4096}
	cmd.Stdout = b
	cmd.Stderr = b
	if err := process.Run(ctx, cmd); err != nil {
		if errors.Is(err, process.ErrUnconfirmed) {
			return "", errors.New("version check process termination unconfirmed; inspect the configured executable")
		}
		return "", errors.New("executable failed its version check")
	}
	return strings.TrimSpace(b.String()), nil
}
func RunDoctor(ctx context.Context, c core.Config, probe, probeFeishu bool) DoctorReport {
	checks := append(deploymentChecks(c), projectEnvChecks(c)...)
	checks = append(checks, projectSettingsChecks(c)...)
	checks = append(checks, approvalPolicyChecks(c)...)
	checks = append(checks, permissionModeChecks(c)...)
	checks = append(checks, feedbackExportChecks(c)...)
	r := DoctorReport{Platform: runtime.GOOS, Runtime: "native-go", Checks: checks, Verification: map[string]string{}, Scope: "Local checks only; no Feishu or model requests were made.", Native: native.Compatibility("")}
	add := func(name string, fn func() (string, error)) {
		if ctx.Err() != nil {
			r.Checks = append(r.Checks, Diagnostic{Name: name, Skipped: true, Detail: "Diagnostic cancelled."})
			return
		}
		detail, err := fn()
		if err != nil {
			detail = core.Redact(err.Error(), config.Secrets(c))
		}
		r.Checks = append(r.Checks, Diagnostic{Name: name, OK: err == nil, Detail: detail})
	}
	add("Git", func() (string, error) { return versionCommand(ctx, "git", "--version") })
	add("Claude Code", func() (string, error) {
		program := c.Runtime.ClaudeExecutable
		if program == "" {
			program = "claude"
		}
		v, e := versionCommand(ctx, program, "--version")
		r.Native = native.Compatibility(v)
		if e != nil || r.Native.Status == "unavailable" {
			return "", errors.New("install native Claude Code or set runtime.claudeExecutable to a working executable")
		}
		return fmt.Sprintf("Actual %s; protocol fixture %s; compatibility %s. Version identity only; native protocol was not executed.", r.Native.Actual, r.Native.Tested, r.Native.Status), nil
	})
	if runtime.GOOS == "windows" && os.Getenv("CLAUDE_CODE_GIT_BASH_PATH") != "" {
		add("configured Git Bash", func() (string, error) {
			p := os.Getenv("CLAUDE_CODE_GIT_BASH_PATH")
			v, e := versionCommand(ctx, p, "--version")
			if e != nil || !strings.EqualFold(filepath.Base(p), "bash.exe") || !strings.Contains(v, "GNU bash, version") {
				return "", errors.New("CLAUDE_CODE_GIT_BASH_PATH must point to working Git for Windows bash.exe")
			}
			return "Configured Git Bash launches.", nil
		})
	}
	add("data directory / SQLite", func() (string, error) {
		if e := os.MkdirAll(c.DataDir, 0700); e != nil {
			return "", errors.New("data directory is not writable")
		}
		dir, e := os.MkdirTemp(c.DataDir, ".doctor-")
		if e != nil {
			return "", errors.New("data directory is not writable")
		}
		defer os.RemoveAll(dir)
		s, e := store.Open(filepath.Join(dir, "probe.sqlite"))
		if e != nil {
			return "", errors.New("temporary SQLite probe failed")
		}
		defer s.Close()
		if _, e = s.Session("probe"); e != nil {
			return "", errors.New("temporary SQLite read/write failed")
		}
		return "Temporary probe succeeded; existing state was not opened.", nil
	})
	for _, p := range c.Projects {
		for name, path := range map[string]string{"cwd": p.CWD, "seedDir": p.SeedDir, "trustedConfigDir": p.TrustedConfigDir} {
			if path != "" {
				add(p.ID+" "+name, func() (string, error) {
					st, e := os.Stat(path)
					if e != nil || !st.IsDir() {
						return "", errors.New("configured directory does not exist")
					}
					return "Existing directory.", nil
				})
			}
		}
		if p.NativeConfig == "inherit" {
			add(p.ID+" inherited native configuration", func() (string, error) {
				dir := os.Getenv("CLAUDE_CONFIG_DIR")
				if dir == "" {
					home, e := os.UserHomeDir()
					if e != nil {
						return "", e
					}
					dir = filepath.Join(home, ".claude")
				}
				st, e := os.Stat(dir)
				if e != nil || !st.IsDir() {
					return "", errors.New("inherited Claude configuration directory must exist")
				}
				return "Directory exists; hooks, MCP and plugins were not executed.", nil
			})
		}
		for _, a := range p.Actions {
			add("action "+p.ID+"/"+a.ID, func() (string, error) {
				st, e := os.Stat(a.Executable)
				if e != nil || !st.Mode().IsRegular() {
					return "", errors.New("action executable must be an existing file")
				}
				if a.CWD != "" {
					st, e = os.Stat(a.CWD)
					if e != nil || !st.IsDir() {
						return "", errors.New("action cwd must be an existing directory")
					}
				}
				return "Configured executable exists; not executed.", nil
			})
		}
	}
	for _, group := range c.Groups {
		for root, path := range group.ThreadWorkspaces {
			add(group.ChatID+" thread workspace "+root, func() (string, error) {
				st, err := os.Stat(path)
				if err != nil || !st.IsDir() {
					return "", errors.New("configured topic workspace directory does not exist")
				}
				return "Existing topic directory; no files copied or worktree created.", nil
			})
		}
	}
	localOK := true
	for _, c := range r.Checks {
		localOK = localOK && c.OK
	}
	for _, key := range []string{"local", "configuration", "nativeExecutable", "nativeProtocol", "model", "modelStreaming", "modelTools", "feishuAuth", "feishuIdentity", "messageDelivery", "cardCallbacks", "fileDelivery", "nativeTools", "feishuReconnect", "storageRows"} {
		r.Verification[key] = "not_checked"
	}
	r.Verification["local"] = "failed"
	if localOK {
		r.Verification["local"] = "passed"
	}
	r.Verification["configuration"] = "passed"
	for _, check := range deploymentChecks(c) {
		if !check.OK {
			r.Verification["configuration"] = "failed"
		}
	}
	r.Verification["nativeExecutable"] = "failed"
	if r.Native.Actual != "" {
		r.Verification["nativeExecutable"] = "passed"
	}
	r.Storage = inspectStorage(ctx, c.DataDir)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if probeFeishu {
		secret := os.Getenv(c.AppSecretEnv)
		if placeholder(c.AppID) || strings.TrimSpace(secret) == "" || placeholder(secret) {
			r.Verification["feishuAuth"] = "blocked"
			r.Verification["feishuIdentity"] = "blocked"
			r.Checks = append(r.Checks, Diagnostic{Name: "Feishu identity probe", Skipped: true, Detail: "Replace application and credential placeholders first."})
		} else {
			r.Scope = "Local checks and explicitly requested remote probes; not full deployment acceptance."
			auth, identity, e := probeIdentity(ctx, client, c.AppID, secret, "https://open.feishu.cn")
			r.Verification["feishuAuth"] = auth
			r.Verification["feishuIdentity"] = identity
			detail := "Application authentication and bot identity verified."
			if e != nil {
				detail = e.Error()
			}
			r.Checks = append(r.Checks, Diagnostic{Name: "Feishu identity probe", OK: e == nil, Detail: detail})
		}
	}
	if probe {
		ready := true
		for _, check := range deploymentChecks(c) {
			ready = ready && check.OK
		}
		if !ready {
			r.Verification["model"] = "blocked"
			r.Checks = append(r.Checks, Diagnostic{Name: "model endpoint basic probe", Skipped: true, Detail: "Correct configuration and credentials first; no request sent."})
		} else {
			r.Scope = "Local checks and explicitly requested remote probes; not full deployment acceptance."
			e := probeModel(ctx, client, c)
			r.Verification["model"] = "passed"
			detail := "Basic Messages response verified; streaming and tools were not tested."
			if e != nil {
				r.Verification["model"] = "failed"
				detail = e.Error()
			}
			r.Checks = append(r.Checks, Diagnostic{Name: "model endpoint basic probe", OK: e == nil, Detail: detail})
		}
	}
	r.Ready = true
	for _, check := range r.Checks {
		r.Ready = r.Ready && check.OK
	}
	for i := range r.Checks {
		if !r.Checks[i].OK {
			r.Checks[i].Remedy = diagnosticRemedy(r.Checks[i].Name)
		}
	}
	r.NextSteps = diagnosticNextSteps(r, probe, probeFeishu)
	return r
}
func decodeProbe(resp *http.Response, dst any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d; response body is not displayed", resp.StatusCode)
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(data) > 65536 {
		return errors.New("probe response failed or exceeded 64 KiB")
	}
	if json.Unmarshal(data, dst) != nil {
		return errors.New("endpoint did not return the expected JSON response")
	}
	return nil
}
func probeModel(ctx context.Context, client *http.Client, c core.Config) error {
	model := regexp.MustCompile(`(?i)\[1m\]$`).ReplaceAllString(c.Model.Model, "")
	body, _ := json.Marshal(core.Args{"model": model, "max_tokens": 256, "messages": []core.Args{{"role": "user", "content": "Reply OK."}}})
	req, e := http.NewRequestWithContext(ctx, "POST", c.Model.BaseURL+"/v1/messages", bytes.NewReader(body))
	if e != nil {
		return errors.New("invalid model endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+os.Getenv(c.Model.TokenEnv))
	headers, e := config.ModelHeaders(c)
	if e != nil {
		return e
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("model endpoint request failed")
	}
	var value struct {
		Type, Role string
		Content    []struct{ Type, Text string }
	}
	if e = decodeProbe(resp, &value); e != nil {
		return e
	}
	if value.Type != "message" || value.Role != "assistant" {
		return errors.New("endpoint did not return an assistant Messages response")
	}
	for _, b := range value.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return nil
		}
	}
	return errors.New("assistant response has no text")
}
func probeIdentity(ctx context.Context, client *http.Client, app, secret, base string) (string, string, error) {
	body, _ := json.Marshal(core.Args{"app_id": app, "app_secret": secret})
	req, e := http.NewRequestWithContext(ctx, "POST", base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if e != nil {
		return "failed", "blocked", errors.New("invalid Feishu identity endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := client.Do(req)
	if e != nil {
		return "failed", "blocked", errors.New("Feishu authentication request failed")
	}
	var auth struct {
		Code  int    `json:"code"`
		Token string `json:"tenant_access_token"`
	}
	if e = decodeProbe(resp, &auth); e != nil {
		return "failed", "blocked", e
	}
	if auth.Code != 0 || auth.Token == "" {
		return "failed", "blocked", fmt.Errorf("Feishu authentication failed (code %d)", auth.Code)
	}
	req, e = http.NewRequestWithContext(ctx, "GET", base+"/open-apis/bot/v3/info", nil)
	if e != nil {
		return "passed", "failed", e
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, e = client.Do(req)
	if e != nil {
		return "passed", "failed", errors.New("Feishu bot identity request failed")
	}
	var identity struct {
		Code int
		Bot  struct {
			OpenID string `json:"open_id"`
		}
	}
	if e = decodeProbe(resp, &identity); e != nil {
		return "passed", "failed", e
	}
	if identity.Code != 0 || identity.Bot.OpenID == "" {
		return "passed", "failed", fmt.Errorf("Feishu bot identity unavailable (code %d)", identity.Code)
	}
	return "passed", "passed", nil
}
