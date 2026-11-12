package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/workspace"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var parameterTemplate = regexp.MustCompile(`\{\{([^}]+)\}\}`)
var fileParameterName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)

// ActionArguments performs value substitution only after policy validation; it
// never passes user-controlled shell syntax to an interpreter.
func ActionArguments(spec core.ActionSpec, input core.Args, snapshots map[string]string) ([]string, error) {
	return actionArguments(spec, input, snapshots, "")
}

// actionArguments is the same policy check with one explicit exemption for the
// preContext parameter. The action template must place that parameter after a
// literal "--"; this exemption does not relax any other parameter.
func actionArguments(spec core.ActionSpec, input core.Args, snapshots map[string]string, allowLeadingDash string) ([]string, error) {
	for name := range input {
		if _, ok := spec.Parameters[name]; !ok {
			return nil, fmt.Errorf("unknown parameter: %s", name)
		}
	}
	values := map[string]string{}
	for name, p := range spec.Parameters {
		value, ok := input[name].(string)
		if !ok || len([]rune(value)) > p.MaxLength || strings.ContainsAny(value, "\x00\r\n") || (strings.HasPrefix(value, "-") && name != allowLeadingDash) {
			return nil, fmt.Errorf("invalid parameter: %s", name)
		}
		if len(p.Enum) > 0 && !contains(p.Enum, value) {
			return nil, fmt.Errorf("invalid option: %s", name)
		}
		if p.Pattern != "" {
			re, e := regexp.Compile("^(?:" + p.Pattern + ")$")
			if e != nil || !re.MatchString(value) {
				return nil, fmt.Errorf("parameter does not match policy: %s", name)
			}
		}
		values[name] = value
	}
	args := make([]string, len(spec.Args))
	for i, arg := range spec.Args {
		unknown := false
		args[i] = parameterTemplate.ReplaceAllStringFunc(arg, func(match string) string {
			key := match[2 : len(match)-2]
			if v, ok := snapshots[key]; ok {
				return v
			}
			v, ok := values[key]
			if !ok {
				unknown = true
			}
			return v
		})
		if unknown {
			return nil, errors.New("action template references an unknown parameter")
		}
	}
	return args, nil
}

const directApprovalError = "需要管理员发起"

// directApprovalRejection reports why an approval:none action may not run. The
// action executes only when the request is active and every sender in this
// turn is a project maintainer.
func directApprovalRejection(requestContext *core.RequestContext, maintainers []string) string {
	if requestContext == nil || len(requestContext.Senders) == 0 {
		return "inactive"
	}
	if len(maintainers) == 0 {
		return "no_maintainers"
	}
	for _, sender := range requestContext.Senders {
		if !contains(maintainers, sender) {
			return "non_maintainer"
		}
	}
	return ""
}

func cleanActionEnv(extra map[string]string) map[string]string {
	env := map[string]string{}
	// Keep only a small, non-secret base environment. On Windows, tools such
	// as lark-cli and PowerShell also need the user/system directory variables
	// below; action vars can still override any of them explicitly.
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "LANG", "LC_ALL", "TZ", "TEMP", "TMP", "LOCALAPPDATA", "APPDATA", "USERPROFILE", "SystemDrive", "ProgramData", "ALLUSERSPROFILE"} {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	for key, v := range extra {
		env[key] = v
	}
	return env
}
func envList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// addRequestContextEnvironment exposes the request's human origin to the
// fixed-operation subprocess. Empty values are omitted.
func addRequestContextEnvironment(env map[string]string, requestContext *core.RequestContext) {
	if requestContext == nil {
		return
	}
	set := func(key, value string) {
		if value != "" {
			env[key] = value
		}
	}
	set("FEISHU_REQUEST_CHAT_ID", requestContext.ChatID)
	set("FEISHU_REQUEST_CHAT_TYPE", requestContext.ChatType)
	set("FEISHU_REQUEST_THREAD_ID", requestContext.ThreadID)
	set("FEISHU_REQUEST_MESSAGE_ID", requestContext.MessageID)
	set("FEISHU_REQUEST_SENDER_OPEN_ID", requestContext.SenderOpenID)
	set("FEISHU_REQUEST_SENDER_NAME", requestContext.SenderName)
	if len(requestContext.Senders) > 0 {
		env["FEISHU_REQUEST_SENDERS"] = strings.Join(requestContext.Senders, ",")
	}
}

type boundedOutput struct {
	mu       sync.Mutex
	b        bytes.Buffer
	max      int
	overflow chan struct{}
	once     sync.Once
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.max - b.b.Len()
	if len(p) > remaining {
		if remaining > 0 {
			b.b.Write(p[:remaining])
		}
		b.once.Do(func() { close(b.overflow) })
	} else {
		b.b.Write(p)
	}
	return n, nil
}
func (b *boundedOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
func runActionCommand(ctx context.Context, executable string, args []string, cwd string, env map[string]string, timeoutMS int64) (int, string, error) {
	if e := ctx.Err(); e != nil {
		return 0, "", e
	}
	cmd := exec.Command(executable, args...)
	cmd.Dir = cwd
	cmd.Env = envList(env)
	cmd.WaitDelay = 2 * time.Second
	output := &boundedOutput{max: 1024 * 1024, overflow: make(chan struct{})}
	cmd.Stdout = output
	cmd.Stderr = output
	process.Prepare(cmd)
	if e := cmd.Start(); e != nil {
		return 0, "", e
	}
	tree, e := process.Attach(cmd)
	if e != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, "", fmt.Errorf("process termination unconfirmed after ownership attachment failure: %w", e)
	}
	defer tree.Close()
	done := make(chan struct{})
	waited := make(chan error, 1)
	go func() { err := cmd.Wait(); waited <- err; close(done) }()
	if timeoutMS <= 0 {
		timeoutMS = 60000
	}
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	reason := ""
	select {
	case e = <-waited:
		select {
		case <-output.overflow:
			reason = "output size limit"
		default:
		}
		if reason != "" {
			// Both completion and overflow may be ready. Oversized output
			// must not become a successful action by select's random choice.
		} else if errors.Is(e, exec.ErrWaitDelay) {
			reason = "process pipes remained open"
		} else {
			code := 0
			if e != nil {
				var exit *exec.ExitError
				if !errors.As(e, &exit) {
					return 0, output.String(), e
				}
				code = exit.ExitCode()
			}
			// A short action is complete only when its owned descendants
			// are gone too, even if they closed stdio before the leader.
			if err := tree.Terminate(done); err != nil {
				return code, output.String(), err
			}
			return code, output.String(), nil
		}
	case <-ctx.Done():
		reason = "operation cancelled"
	case <-timer.C:
		reason = "operation timed out"
	case <-output.overflow:
		reason = "output size limit"
	}
	if err := tree.Terminate(done); err != nil {
		return 0, output.String(), fmt.Errorf("%s; process termination unconfirmed: %w", reason, err)
	}
	select {
	case <-done:
		return 0, output.String(), errors.New(reason)
	case <-time.After(5 * time.Second):
		return 0, output.String(), fmt.Errorf("%s; process termination unconfirmed", reason)
	}
}
func (g *Gateway) executeAction(ctx context.Context, r core.Room, session string, p core.Project, id string, input core.Args, w core.Workspace, requestContext *core.RequestContext) (core.Args, error) {
	return g.executeActionPolicy(ctx, r, session, p, id, input, w, requestContext, "", false, nil)
}

func (g *Gateway) executeActionWithPolicy(ctx context.Context, r core.Room, session string, p core.Project, id string, input core.Args, w core.Workspace, requestContext *core.RequestContext, allowLeadingDash string) (core.Args, error) {
	return g.executeActionPolicy(ctx, r, session, p, id, input, w, requestContext, allowLeadingDash, false, nil)
}

func (g *Gateway) executeActionTainted(ctx context.Context, r core.Room, session string, p core.Project, id string, input core.Args, w core.Workspace, requestContext *core.RequestContext, tainted bool, targets []string) (core.Args, error) {
	return g.executeActionPolicy(ctx, r, session, p, id, input, w, requestContext, "", tainted, targets)
}

func (g *Gateway) executeActionPolicy(ctx context.Context, r core.Room, session string, p core.Project, id string, input core.Args, w core.Workspace, requestContext *core.RequestContext, allowLeadingDash string, tainted bool, taintTargets []string) (core.Args, error) {
	var spec *core.ActionSpec
	for _, a := range p.Actions {
		if a.ID == id {
			v := a
			spec = &v
			break
		}
	}
	if spec == nil {
		return nil, errors.New("action not allowed in this project")
	}
	if _, e := actionArguments(*spec, input, nil, allowLeadingDash); e != nil {
		return nil, e
	}
	approvalMode := "card"
	if spec.ReadOnly {
		approvalMode = "readonly"
	} else if spec.Approval == "none" {
		approvalMode = "none"
	}
	direct := !spec.ReadOnly && approvalMode == "none"
	run := core.NewID()
	if direct {
		if reason := directApprovalRejection(requestContext, p.Maintainers); reason != "" {
			detail := core.Args{"run": run, "action": id, "reason": reason}
			if requestContext != nil {
				if requestContext.SenderOpenID != "" {
					detail["requester"] = requestContext.SenderOpenID
				}
				if requestContext.MessageID != "" {
					detail["messageId"] = requestContext.MessageID
				}
			}
			g.audit(r.Key, "action_direct_rejected", detail)
			return core.Args{"ok": false, "error": directApprovalError}, nil
		}
	}
	staging := filepath.Join(g.config.DataDir, "action-inputs", run)
	defer os.RemoveAll(staging)
	snapshots := map[string]string{}
	digests := map[string]string{}
	for name, param := range spec.Parameters {
		if !param.WorkspaceFile {
			continue
		}
		if !fileParameterName.MatchString(name) {
			return nil, errors.New("invalid file parameter name")
		}
		path, e := workspace.TranslateFile(w, stringArg(input, name))
		if e != nil {
			return nil, e
		}
		target := filepath.Join(staging, name)
		if e = workspace.Snapshot(w.Work, path, target, 10*1024*1024); e != nil {
			return nil, e
		}
		f, e := os.Open(target)
		if e != nil {
			return nil, e
		}
		digest := sha256.New()
		_, e = io.Copy(digest, f)
		f.Close()
		if e != nil {
			return nil, e
		}
		snapshots[name] = target
		digests[name] = hex.EncodeToString(digest.Sum(nil))
	}
	args, e := actionArguments(*spec, input, snapshots, allowLeadingDash)
	if e != nil {
		return nil, e
	}
	digest := core.Hash(jsonText(core.Args{"id": id, "input": input, "fileDigests": digests}))
	var approvalRequest, approver, taintRequest, taintApprover string
	var approvedAt int64
	if approvalMode == "card" {
		policy := approvalPolicyForAction(p, *spec, requestContext)
		summary := "项目：" + p.ID + "\n" + requesterLine(requestContext) + "\n"
		if len(spec.Approvers) > 0 {
			summary += "审批人：指定审批人\n"
		}
		summary += "操作：" + spec.Description + "\n参数：`" + cut(jsonText(g.redactArgs(input)), 4000) + "`\n文件快照 SHA256：`" + jsonText(digests) + "`\n摘要：`" + digest[:16] + "`"
		if tainted {
			// Reading another group adds a confirmation step for the requester.
			// It must not replace or weaken the action's original approvers or
			// self-approval rule.
			confirm := taintedApprovalPolicy(approvalPolicyForTool(p, requestContext), r.DMUser, taintTargets)
			confirmSummary := "本轮读取过其他群的内容；该操作还需要原审批人批准。\n" + summary
			decision, e := g.ask(ctx, r, session, id, input, confirmSummary, nil, confirm)
			if e != nil {
				return nil, e
			}
			if !boolArg(decision, "approved") {
				return core.Args{"ok": false, "error": stringArg(decision, "reason")}, nil
			}
			taintRequest, taintApprover = stringArg(decision, "request"), stringArg(decision, "actor")
		}
		decision, e := g.ask(ctx, r, session, id, input, summary, nil, policy)
		if e != nil {
			return nil, e
		}
		if !boolArg(decision, "approved") {
			return core.Args{"ok": false, "error": stringArg(decision, "reason")}, nil
		}
		approvalRequest, approver = stringArg(decision, "request"), stringArg(decision, "actor")
		approvedAt, _ = numberArg(decision, "settledAt")
	}
	if ctx.Err() != nil {
		return core.Args{"ok": false, "error": "cancelled"}, nil
	}
	env := cleanActionEnv(spec.Vars)
	secrets := append([]string(nil), g.secrets...)
	for key, name := range spec.Env {
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("missing configured secret: %s", name)
		}
		env[key] = value
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	addRequestContextEnvironment(env, requestContext)
	if approvalMode == "card" {
		set := func(key, value string) {
			if value != "" {
				env[key] = value
			}
		}
		set("FEISHU_ACTION_REQUEST_ID", approvalRequest)
		set("FEISHU_ACTION_APPROVER_OPEN_ID", approver)
		if approvedAt > 0 {
			env["FEISHU_ACTION_APPROVED_AT"] = time.UnixMilli(approvedAt).UTC().Format(time.RFC3339)
		}
		if tainted {
			set("FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID", taintApprover)
		}
	}
	execution, e := g.beginAction(ctx, r.Key, run)
	if e != nil {
		return nil, e
	}
	unconfirmed := false
	defer func() { g.endAction(run, execution, unconfirmed) }()
	var requester, contextJSON any
	if requestContext != nil {
		if requestContext.SenderOpenID != "" {
			requester = requestContext.SenderOpenID
		}
		if data, marshalErr := json.Marshal(requestContext); marshalErr == nil {
			contextJSON = string(data)
		}
	}
	if _, e = g.store.DB.Exec("INSERT INTO action_runs(id,room,session,action,digest,state,at,requester,request_context,request,approver,taint_request,approval) VALUES(?,?,?,?,?,'running',?,?,?,?,?,?,?)", run, r.Key, session, id, digest, time.Now().UnixMilli(), requester, contextJSON, nullString(approvalRequest), nullString(approver), nullString(taintRequest), approvalMode); e != nil {
		return nil, e
	}
	detail := core.Args{"run": run, "action": id, "digest": digest, "readOnly": spec.ReadOnly, "approval": approvalMode}
	if direct {
		detail["tainted"] = tainted
	}
	if requestContext != nil {
		if requestContext.SenderOpenID != "" {
			detail["requester"] = requestContext.SenderOpenID
		}
		if requestContext.MessageID != "" {
			detail["messageId"] = requestContext.MessageID
		}
	}
	g.audit(r.Key, "action_started", detail)
	code, output, e := g.actionRunner(ctx, spec.Executable, args, spec.CWD, env, spec.TimeoutMS)
	if e != nil {
		_, _ = g.store.DB.Exec("UPDATE action_runs SET state='uncertain' WHERE id=?", run)
		if errors.Is(e, process.ErrUnconfirmed) || strings.Contains(e.Error(), "process termination unconfirmed") {
			unconfirmed = true
			g.audit(r.Key, "action_termination_unconfirmed", core.Args{"run": run, "action": id})
		}
		return nil, e
	}
	state := "failed"
	if code == 0 {
		state = "done"
	}
	if _, e = g.store.DB.Exec("UPDATE action_runs SET state=? WHERE id=?", state, run); e != nil {
		return nil, e
	}
	g.audit(r.Key, "action_finished", core.Args{"run": run, "action": id, "code": code})
	return core.Args{"ok": code == 0, "code": code, "output": core.Redact(output, secrets)}, nil
}
