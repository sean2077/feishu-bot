package gateway

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
)

func TestActionApprovalExecutesTheApprovedFileSnapshot(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish a file", Executable: "fixture", Args: []string{"{{file}}"}, Parameters: map[string]core.Parameter{"file": {MaxLength: 200, WorkspaceFile: true}}}}
	})
	seen := make(chan string, 1)
	f.g.actionRunner = func(_ context.Context, executable string, args []string, _ string, _ map[string]string, _ int64) (int, string, error) {
		b, e := os.ReadFile(args[0])
		seen <- string(b)
		return 0, string(b), e
	}
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	s.mu.Lock()
	work := s.workspace.Work
	s.mu.Unlock()
	path := filepath.Join(work, "document.txt")
	if e := os.WriteFile(path, []byte("approved original"), 0600); e != nil {
		t.Fatal(e)
	}
	w.emit(core.WorkerEvent{Type: "request", ID: "publish-request", Kind: "action", Name: "publish", Input: core.Args{"file": "document.txt"}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	approval := cardValue(card.card, "批准一次")
	waitApprovalBinding(t, f, card, stringArg(approval, "request"))
	if !strings.Contains(jsonText(card.card), "SHA256") {
		t.Fatal("approval missing content digest")
	}
	if e := os.WriteFile(path, []byte("changed after approval prompt"), 0600); e != nil {
		t.Fatal(e)
	}
	_, e := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: approval})
	if e != nil {
		t.Fatal(e)
	}
	a := response(t, w)
	if !boolArg(a, "ok") {
		t.Fatal("approved action failed", a)
	}
	select {
	case content := <-seen:
		if content != "approved original" {
			t.Fatal("executed mutable source instead of approved snapshot", content)
		}
	case <-time.After(time.Second):
		t.Fatal("action did not execute")
	}
	var state string
	if e = f.s.DB.QueryRow("SELECT state FROM action_runs").Scan(&state); e != nil || state != "done" {
		t.Fatal("action result not committed", state, e)
	}
}
func TestActionApproverEnvironmentAndRunMetadata(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "publish", Description: "Publish", Executable: "fixture"}}
	})
	envs := make(chan map[string]string, 1)
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, env map[string]string, _ int64) (int, string, error) {
		copy := make(map[string]string, len(env))
		for key, value := range env {
			copy[key] = value
		}
		envs <- copy
		return 0, "", nil
	}
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "publish-request", Kind: "action", Name: "publish", Input: core.Args{}})
	card, value := approvalCardFor(t, f)
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("approval toast = %q", got)
	}
	if result := response(t, w); !boolArg(result, "ok") {
		t.Fatalf("action result = %#v", result)
	}
	env := <-envs
	requestID := stringArg(value, "request")
	if env["FEISHU_ACTION_REQUEST_ID"] != requestID || env["FEISHU_ACTION_APPROVER_OPEN_ID"] != "owner" {
		t.Fatalf("action approval env = %#v", env)
	}
	if _, err := time.Parse(time.RFC3339, env["FEISHU_ACTION_APPROVED_AT"]); err != nil {
		t.Fatalf("approvedAt = %q: %v", env["FEISHU_ACTION_APPROVED_AT"], err)
	}
	if _, ok := env["FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID"]; ok {
		t.Fatalf("non-tainted action carried taint confirmer: %#v", env)
	}
	var runRequest, runApprover, approval string
	if err := f.s.DB.QueryRow("SELECT request,approver,approval FROM action_runs").Scan(&runRequest, &runApprover, &approval); err != nil {
		t.Fatal(err)
	}
	var actor string
	if err := f.s.DB.QueryRow("SELECT actor FROM requests WHERE id=?", requestID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if runRequest != requestID || runApprover != "owner" || actor != "owner" || approval != "card" {
		t.Fatalf("run metadata = %q %q actor=%q approval=%q", runRequest, runApprover, actor, approval)
	}
}

func TestCleanActionEnvKeepsWindowsPathsAndHonorsOverrides(t *testing.T) {
	keys := []string{"LOCALAPPDATA", "APPDATA", "USERPROFILE", "SystemDrive", "ProgramData", "ALLUSERSPROFILE"}
	for _, key := range keys {
		t.Setenv(key, `C:\inherited\`+key)
	}
	t.Setenv("EXAMPLE_ACTION_SECRET", "secret")
	env := cleanActionEnv(nil)
	for _, key := range keys {
		if env[key] != `C:\inherited\`+key {
			t.Fatalf("base environment lost %s: %#v", key, env)
		}
	}
	if _, ok := env["EXAMPLE_ACTION_SECRET"]; ok {
		t.Fatal("unrelated environment variable was inherited")
	}
	override := cleanActionEnv(map[string]string{"USERPROFILE": `D:\action-home`, "LOCALAPPDATA": `D:\action-local`})
	if override["USERPROFILE"] != `D:\action-home` || override["LOCALAPPDATA"] != `D:\action-local` {
		t.Fatalf("action vars did not override inherited values: %#v", override)
	}
	if override["APPDATA"] != `C:\inherited\APPDATA` {
		t.Fatalf("unrelated inherited value changed: %#v", override)
	}
}

func TestReadOnlyActionOmitsApproverEnvironment(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "check", Description: "Check", Executable: "fixture", ReadOnly: true}}
	})
	envs := make(chan map[string]string, 1)
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, env map[string]string, _ int64) (int, string, error) {
		envs <- env
		return 0, "", nil
	}
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "check-request", Kind: "action", Name: "check", Input: core.Args{}})
	if result := response(t, w); !boolArg(result, "ok") {
		t.Fatalf("readonly action = %#v", result)
	}
	for key := range <-envs {
		if strings.HasPrefix(key, "FEISHU_ACTION_") {
			t.Fatalf("readOnly action received %s", key)
		}
	}
	var request, approver, taintRequest any
	var approval string
	if err := f.s.DB.QueryRow("SELECT request,approver,taint_request,approval FROM action_runs").Scan(&request, &approver, &taintRequest, &approval); err != nil {
		t.Fatal(err)
	}
	if request != nil || approver != nil || taintRequest != nil || approval != "readonly" {
		t.Fatalf("readOnly action metadata = %#v %#v %#v %q", request, approver, taintRequest, approval)
	}
}

func TestActionApprovalNoneRunsForMaintainersWithoutCard(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "build", Description: "Build", Executable: "fixture", Approval: "none"}}
	})
	var env map[string]string
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, got map[string]string, _ int64) (int, string, error) {
		env = got
		return 0, "built", nil
	}
	requestContext := &core.RequestContext{ChatID: "group", ChatType: "group", MessageID: "one", SenderOpenID: "owner", SenderName: "Owner", Senders: []string{"owner"}}
	result, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "build", core.Args{}, core.Workspace{}, requestContext)
	if err != nil || !boolArg(result, "ok") {
		t.Fatalf("direct action = %#v %v", result, err)
	}
	if f.tr.cardCount() != 0 {
		t.Fatal("direct action created an approval card")
	}
	if env["FEISHU_REQUEST_SENDERS"] != "owner" || env["FEISHU_REQUEST_MESSAGE_ID"] != "one" || env["FEISHU_REQUEST_SENDER_OPEN_ID"] != "owner" {
		t.Fatalf("request env = %#v", env)
	}
	for _, key := range []string{"FEISHU_ACTION_REQUEST_ID", "FEISHU_ACTION_APPROVER_OPEN_ID", "FEISHU_ACTION_APPROVED_AT", "FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID"} {
		if _, ok := env[key]; ok {
			t.Fatalf("direct action received %s", key)
		}
	}
	var approval string
	var request, approver, taint any
	if err := f.s.DB.QueryRow("SELECT approval,request,approver,taint_request FROM action_runs WHERE action='build'").Scan(&approval, &request, &approver, &taint); err != nil {
		t.Fatal(err)
	}
	if approval != "none" || request != nil || approver != nil || taint != nil {
		t.Fatalf("run metadata = %q %#v %#v %#v", approval, request, approver, taint)
	}
	detail := auditDetail(t, f, "action_started")
	if stringArg(detail, "approval") != "none" || detail["tainted"] != false {
		t.Fatalf("action_started detail = %#v", detail)
	}
}

func TestActionApprovalNoneRejectsNonMaintainerAndInactiveRequests(t *testing.T) {
	cases := []struct {
		name    string
		context *core.RequestContext
		reason  string
	}{
		{name: "inactive", context: nil, reason: "inactive"},
		{name: "empty", context: &core.RequestContext{ChatID: "group", MessageID: "empty"}, reason: "inactive"},
		{name: "member", context: &core.RequestContext{Senders: []string{"member"}, SenderOpenID: "member", MessageID: "member-message"}, reason: "non_maintainer"},
		{name: "mixed", context: &core.RequestContext{Senders: []string{"owner", "member"}}, reason: "non_maintainer"},
		{name: "other-bot", context: &core.RequestContext{Senders: []string{"owner", "cli_other"}}, reason: "non_maintainer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(c *core.Config) {
				c.Projects[0].Actions = []core.ActionSpec{{ID: "build", Description: "Build", Executable: "fixture", Approval: "none"}}
			})
			called := false
			f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
				called = true
				return 0, "", nil
			}
			result, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "build", core.Args{}, core.Workspace{}, tc.context)
			if err != nil {
				t.Fatal(err)
			}
			if boolArg(result, "ok") || stringArg(result, "error") != "需要管理员发起" {
				t.Fatalf("rejection = %#v", result)
			}
			if called {
				t.Fatal("rejected direct action executed")
			}
			if f.tr.cardCount() != 0 {
				t.Fatal("rejected direct action created an approval card")
			}
			var runs int
			if err := f.s.DB.QueryRow("SELECT count(*) FROM action_runs").Scan(&runs); err != nil || runs != 0 {
				t.Fatalf("rejected action wrote %d runs: %v", runs, err)
			}
			var requests int
			if err := f.s.DB.QueryRow("SELECT count(*) FROM requests").Scan(&requests); err != nil || requests != 0 {
				t.Fatalf("rejected action wrote %d approval requests: %v", requests, err)
			}
			detail := auditDetail(t, f, "action_direct_rejected")
			if stringArg(detail, "reason") != tc.reason || stringArg(detail, "action") != "build" || stringArg(detail, "approval") != "none" {
				t.Fatalf("rejection audit = %#v", detail)
			}
			if tc.context != nil && tc.context.SenderOpenID != "" && stringArg(detail, "requester") != tc.context.SenderOpenID {
				t.Fatalf("rejection audit lost requester = %#v", detail)
			}
			if tc.context != nil && tc.context.MessageID != "" && stringArg(detail, "messageId") != tc.context.MessageID {
				t.Fatalf("rejection audit lost message ID = %#v", detail)
			}
		})
	}
}

func TestActionApprovalNoneStaysDirectOnTaintedTurn(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "build", Description: "Build", Executable: "fixture", Approval: "none"}}
	})
	var env map[string]string
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, got map[string]string, _ int64) (int, string, error) {
		env = got
		return 0, "built", nil
	}
	requestContext := &core.RequestContext{ChatID: "group", ChatType: "group", MessageID: "one", SenderOpenID: "owner", Senders: []string{"owner"}}
	result, err := f.g.executeActionTainted(context.Background(), f.room(), "session", f.c.Projects[0], "build", core.Args{}, core.Workspace{}, requestContext, true, []string{"other"})
	if err != nil || !boolArg(result, "ok") {
		t.Fatalf("tainted direct action = %#v %v", result, err)
	}
	if f.tr.cardCount() != 0 {
		t.Fatal("tainted direct action created an approval card")
	}
	if _, ok := env["FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID"]; ok {
		t.Fatalf("direct action received a taint confirmer: %#v", env)
	}
	detail := auditDetail(t, f, "action_started")
	if detail["tainted"] != true || stringArg(detail, "approval") != "none" {
		t.Fatalf("tainted action_started detail = %#v", detail)
	}
	var approval string
	if err := f.s.DB.QueryRow("SELECT approval FROM action_runs WHERE action='build'").Scan(&approval); err != nil || approval != "none" {
		t.Fatalf("tainted run approval = %q %v", approval, err)
	}
}

func TestActionApprovalNoneTaintedTurnStillRejectsNonMaintainer(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "build", Description: "Build", Executable: "fixture", Approval: "none"}}
	})
	called := false
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
		called = true
		return 0, "", nil
	}
	requestContext := &core.RequestContext{ChatID: "group", MessageID: "member-message", SenderOpenID: "member", Senders: []string{"member"}}
	result, err := f.g.executeActionTainted(context.Background(), f.room(), "session", f.c.Projects[0], "build", core.Args{}, core.Workspace{}, requestContext, true, []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if boolArg(result, "ok") || stringArg(result, "error") != "需要管理员发起" || called {
		t.Fatalf("tainted non-maintainer result = %#v executed=%t", result, called)
	}
	if f.tr.cardCount() != 0 {
		t.Fatal("tainted rejection created an approval card")
	}
}

func TestActionApprovalNoneIgnoresApproversAndSelfApproval(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Owners = []string{"owner"}
		c.Projects[0].Approvals.ForbidSelfApproval = true
		c.Projects[0].Actions = []core.ActionSpec{{ID: "build", Description: "Build", Executable: "fixture", Approval: "none", Approvers: []string{"other"}}}
	})
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
		return 0, "built", nil
	}
	requestContext := &core.RequestContext{ChatID: "group", MessageID: "one", SenderOpenID: "owner", Senders: []string{"owner"}}
	result, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "build", core.Args{}, core.Workspace{}, requestContext)
	if err != nil || !boolArg(result, "ok") {
		t.Fatalf("direct action with approvers = %#v %v", result, err)
	}
	if f.tr.cardCount() != 0 {
		t.Fatal("approval:none asked the configured approvers")
	}
}

func TestActionApprovalNoneDoesNotAffectReadOnly(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "check", Description: "Check", Executable: "fixture", ReadOnly: true, Approval: "none"}}
	})
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
		return 0, "checked", nil
	}
	result, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "check", core.Args{}, core.Workspace{}, nil)
	if err != nil || !boolArg(result, "ok") {
		t.Fatalf("readOnly action with approval:none = %#v %v", result, err)
	}
	var approval string
	if err := f.s.DB.QueryRow("SELECT approval FROM action_runs WHERE action='check'").Scan(&approval); err != nil || approval != "readonly" {
		t.Fatalf("readOnly run approval = %q %v", approval, err)
	}
}

func TestActionApprovalOpenRunsForMemberWithoutCard(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "ota", Description: "OTA", Executable: "fixture", Approval: "open"}}
	})
	var env map[string]string
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, got map[string]string, _ int64) (int, string, error) {
		env = got
		return 0, "queued", nil
	}
	requestContext := &core.RequestContext{ChatID: "group", ChatType: "group", MessageID: "member-message", SenderOpenID: "member", Senders: []string{"member"}}
	result, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "ota", core.Args{}, core.Workspace{}, requestContext)
	if err != nil || !boolArg(result, "ok") {
		t.Fatalf("open action = %#v %v", result, err)
	}
	if f.tr.cardCount() != 0 {
		t.Fatal("open action created an approval card")
	}
	if env["FEISHU_REQUEST_SENDERS"] != "member" || env["FEISHU_REQUEST_MESSAGE_ID"] != "member-message" {
		t.Fatalf("request env = %#v", env)
	}
	for _, key := range []string{"FEISHU_ACTION_REQUEST_ID", "FEISHU_ACTION_APPROVER_OPEN_ID", "FEISHU_ACTION_APPROVED_AT", "FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID"} {
		if _, ok := env[key]; ok {
			t.Fatalf("open action received %s", key)
		}
	}
	var approval string
	var request, approver, taint any
	if err := f.s.DB.QueryRow("SELECT approval,request,approver,taint_request FROM action_runs WHERE action='ota'").Scan(&approval, &request, &approver, &taint); err != nil {
		t.Fatal(err)
	}
	if approval != "open" || request != nil || approver != nil || taint != nil {
		t.Fatalf("open run metadata = %q %#v %#v %#v", approval, request, approver, taint)
	}
	detail := auditDetail(t, f, "action_started")
	if stringArg(detail, "approval") != "open" || detail["tainted"] != false {
		t.Fatalf("open action_started detail = %#v", detail)
	}
}

func TestActionApprovalOpenRejectsInactiveAndEmptySenders(t *testing.T) {
	cases := []struct {
		name    string
		context *core.RequestContext
	}{
		{name: "inactive", context: nil},
		{name: "empty", context: &core.RequestContext{ChatID: "group", MessageID: "empty"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(c *core.Config) {
				c.Projects[0].Actions = []core.ActionSpec{{ID: "ota", Description: "OTA", Executable: "fixture", Approval: "open"}}
			})
			called := false
			f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
				called = true
				return 0, "", nil
			}
			result, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "ota", core.Args{}, core.Workspace{}, tc.context)
			if err != nil {
				t.Fatal(err)
			}
			if boolArg(result, "ok") || stringArg(result, "error") != "需要活跃的用户请求" || called {
				t.Fatalf("open rejection = %#v executed=%t", result, called)
			}
			if f.tr.cardCount() != 0 {
				t.Fatal("open rejection created an approval card")
			}
			var runs, requests int
			if err := f.s.DB.QueryRow("SELECT count(*) FROM action_runs").Scan(&runs); err != nil || runs != 0 {
				t.Fatalf("rejected action wrote %d runs: %v", runs, err)
			}
			if err := f.s.DB.QueryRow("SELECT count(*) FROM requests").Scan(&requests); err != nil || requests != 0 {
				t.Fatalf("rejected action wrote %d requests: %v", requests, err)
			}
			detail := auditDetail(t, f, "action_direct_rejected")
			if stringArg(detail, "reason") != "inactive" || stringArg(detail, "action") != "ota" || stringArg(detail, "approval") != "open" {
				t.Fatalf("open rejection audit = %#v", detail)
			}
		})
	}
}

func TestActionApprovalOpenStaysDirectOnTaintedTurn(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].Actions = []core.ActionSpec{{ID: "ota", Description: "OTA", Executable: "fixture", Approval: "open"}}
	})
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
		return 0, "queued", nil
	}
	requestContext := &core.RequestContext{ChatID: "group", MessageID: "member-message", SenderOpenID: "member", Senders: []string{"member"}}
	result, err := f.g.executeActionTainted(context.Background(), f.room(), "session", f.c.Projects[0], "ota", core.Args{}, core.Workspace{}, requestContext, true, []string{"other"})
	if err != nil || !boolArg(result, "ok") {
		t.Fatalf("tainted open action = %#v %v", result, err)
	}
	if f.tr.cardCount() != 0 {
		t.Fatal("tainted open action created an approval card")
	}
	detail := auditDetail(t, f, "action_started")
	if detail["tainted"] != true || stringArg(detail, "approval") != "open" {
		t.Fatalf("tainted open action_started detail = %#v", detail)
	}
	var approval string
	if err := f.s.DB.QueryRow("SELECT approval FROM action_runs WHERE action='ota'").Scan(&approval); err != nil || approval != "open" {
		t.Fatalf("tainted open run approval = %q %v", approval, err)
	}
}

func TestDirectApprovalRejectsBotSenders(t *testing.T) {
	cases := []struct {
		name    string
		senders []string
	}{
		{name: "bot", senders: []string{"cli_other"}},
		{name: "mixed", senders: []string{"member", "cli_other"}},
	}
	for _, mode := range []string{"none", "open"} {
		for _, tc := range cases {
			t.Run(mode+"-"+tc.name, func(t *testing.T) {
				f := newFixture(t, func(c *core.Config) {
					c.Projects[0].Actions = []core.ActionSpec{{ID: "ota", Description: "OTA", Executable: "fixture", Approval: mode}}
				})
				called := false
				f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, _ map[string]string, _ int64) (int, string, error) {
					called = true
					return 0, "", nil
				}
				requestContext := &core.RequestContext{ChatID: "group", MessageID: "bot-message", SenderOpenID: tc.senders[0], Senders: tc.senders, HasBotSender: true}
				result, err := f.g.executeAction(context.Background(), f.room(), "session", f.c.Projects[0], "ota", core.Args{}, core.Workspace{}, requestContext)
				if err != nil {
					t.Fatal(err)
				}
				if boolArg(result, "ok") || stringArg(result, "error") != "机器人消息不能触发写操作" || called {
					t.Fatalf("bot sender result = %#v executed=%t", result, called)
				}
				if f.tr.cardCount() != 0 {
					t.Fatal("bot sender created an approval card")
				}
				detail := auditDetail(t, f, "action_direct_rejected")
				if detail["bot_sender"] != true || stringArg(detail, "approval") != mode || stringArg(detail, "reason") != "bot_sender" {
					t.Fatalf("bot sender audit = %#v", detail)
				}
			})
		}
	}
}

func TestOpenApprovalRejectionReasons(t *testing.T) {
	if reason := openApprovalRejection(nil); reason != "inactive" {
		t.Fatalf("inactive reason = %q", reason)
	}
	if reason := openApprovalRejection(&core.RequestContext{Senders: []string{}}); reason != "inactive" {
		t.Fatalf("empty senders reason = %q", reason)
	}
	if reason := openApprovalRejection(&core.RequestContext{Senders: []string{"member"}}); reason != "" {
		t.Fatalf("member reason = %q", reason)
	}
}

func TestDirectApprovalRejectionReasons(t *testing.T) {
	maintainers := []string{"owner"}
	if reason := directApprovalRejection(nil, maintainers); reason != "inactive" {
		t.Fatalf("inactive reason = %q", reason)
	}
	if reason := directApprovalRejection(&core.RequestContext{Senders: []string{}}, maintainers); reason != "inactive" {
		t.Fatalf("empty senders reason = %q", reason)
	}
	if reason := directApprovalRejection(&core.RequestContext{Senders: []string{"member"}}, maintainers); reason != "non_maintainer" {
		t.Fatalf("member reason = %q", reason)
	}
	if reason := directApprovalRejection(&core.RequestContext{Senders: []string{"owner"}}, nil); reason != "no_maintainers" {
		t.Fatalf("missing maintainers reason = %q", reason)
	}
	if reason := directApprovalRejection(&core.RequestContext{Senders: []string{"owner"}}, maintainers); reason != "" {
		t.Fatalf("maintainer reason = %q", reason)
	}
}

func TestForbidSelfApprovalStillRecordsApprover(t *testing.T) {
	f := actionApprovalFixture(t, []string{"owner", "member"}, []string{"owner"}, true)
	envs := make(chan map[string]string, 1)
	f.g.actionRunner = func(_ context.Context, _ string, _ []string, _ string, env map[string]string, _ int64) (int, string, error) {
		envs <- env
		return 0, "", nil
	}
	f.send(f.message("message-member", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "publish-request", Kind: "action", Name: "publish", Input: core.Args{}})
	card, value := approvalCardFor(t, f)
	if got := clickApproval(t, f, card, value, "member"); got != selfApprovalToast {
		t.Fatalf("self approval toast = %q", got)
	}
	if got := clickApproval(t, f, card, value, "owner"); got != "已批准一次" {
		t.Fatalf("owner approval toast = %q", got)
	}
	if result := response(t, w); !boolArg(result, "ok") {
		t.Fatalf("action result = %#v", result)
	}
	if env := <-envs; env["FEISHU_ACTION_APPROVER_OPEN_ID"] != "owner" {
		t.Fatalf("forbid-self action env = %#v", env)
	}
}

func TestUnconfirmedActionCleanupBlocksNewSessionAndClose(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Runtime.MaxWorkers = 1
		c.Projects[0].Actions = []core.ActionSpec{{ID: "check", Executable: "fixture", ReadOnly: true}}
	})
	f.g.actionRunner = func(context.Context, string, []string, string, map[string]string, int64) (int, string, error) {
		return 0, "", errors.New("operation cancelled; process termination unconfirmed")
	}
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	saved, e := f.s.Session(f.room().Key)
	if e != nil {
		t.Fatal(e)
	}
	w.emit(core.WorkerEvent{Type: "request", ID: "unsafe-action", Kind: "action", Name: "check", Input: core.Args{}})
	a := response(t, w)
	if boolArg(a, "ok") {
		t.Fatal("uncertain action reported success")
	}
	eventually(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		for _, a := range f.g.actions {
			if a.unconfirmed {
				return true
			}
		}
		return false
	})
	if e = f.g.Message(context.Background(), f.message("rotate", "/new", "owner")); e == nil {
		t.Fatal("new session allowed with unknown action process")
	}
	current, e := f.s.Session(f.room().Key)
	if e != nil || current.ID != saved.ID {
		t.Fatal("session rotated", current, e)
	}
	m := f.message("other-input", "work", "member")
	m.ChatID = "other"
	f.send(m)
	if f.starts.Load() != 1 {
		t.Fatal("unknown action did not retain worker capacity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = f.g.Close(ctx); e == nil || !strings.Contains(e.Error(), "action process termination") {
		t.Fatal("close released service lock with unknown action process", e)
	}
	// The simulated process has no OS resources. Clear only its injected fence
	// so the fixture can perform normal final database cleanup.
	f.g.mu.Lock()
	f.g.actions = map[string]*actionExecution{}
	f.g.mu.Unlock()
	for _, s := range f.g.allSlots() {
		s.mu.Lock()
		s.stopUnconfirmed = false
		s.mu.Unlock()
	}
}
func TestApprovalPersistenceFailureDeniesAndReleasesWaiter(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	w.emit(core.WorkerEvent{Type: "request", ID: "request", Kind: "permission", Name: "Bash", Input: core.Args{}})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := f.tr.cardLast()
	approval := cardValue(card.card, "批准一次")
	waitApprovalBinding(t, f, card, stringArg(approval, "request"))
	if _, e := f.s.DB.Exec("CREATE TRIGGER approval_storage_failure BEFORE UPDATE ON requests BEGIN SELECT RAISE(ABORT,'injected write failure'); END"); e != nil {
		t.Fatal(e)
	}
	_, e := f.g.Action(context.Background(), core.CardAction{UserID: "owner", ChatID: "group", MessageID: card.id, Value: approval})
	if e != nil {
		t.Fatal(e)
	}
	a := response(t, w)
	if boolArg(a, "approved") {
		t.Fatal("unstored approval authorized an operation")
	}
	f.g.approvalMu.Lock()
	remaining := len(f.g.approvals)
	f.g.approvalMu.Unlock()
	if remaining != 0 {
		t.Fatal("failed approval retained local waiter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = f.g.Close(ctx); e != nil {
		t.Fatal("storage failure prevented shutdown", e)
	}
}
func TestConfiguredHeaderAndActionSecretsRedactedInCardsAndAudit(t *testing.T) {
	t.Setenv("FIXTURE_ACTION_SECRET", `action"secret`)
	f := newFixture(t, func(c *core.Config) {
		c.Model.ExtraHeaders = map[string]string{"x-private": `header"secret`}
		c.Projects[0].Actions = []core.ActionSpec{{ID: "fixture", Executable: "fixture", Env: map[string]string{"PRIVATE": "FIXTURE_ACTION_SECRET"}}}
	})
	f.send(f.message("one", "work", "member"))
	w := f.worker()
	_ = f.delivery(w)
	payload := core.Args{"values": []any{`action"secret`, `header"secret`}}
	w.emit(core.WorkerEvent{Type: "request", ID: "request", Kind: "permission", Name: "Bash", Input: payload})
	eventually(t, func() bool { return f.tr.cardCount() > 0 })
	card := jsonText(f.tr.cardLast().card)
	if strings.Contains(card, "action") || strings.Contains(card, "header\\") || !strings.Contains(card, "[REDACTED]") {
		t.Fatal("configured secret exposed by approval", card)
	}
	f.g.audit(f.room().Key, "redaction-fixture", payload)
	var raw string
	if e := f.s.DB.QueryRow("SELECT detail FROM audit WHERE event='redaction-fixture'").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(raw, "action") || strings.Contains(raw, "header") || !strings.Contains(raw, "[REDACTED]") {
		t.Fatal("configured secret exposed in audit", raw)
	}
}
func TestActionProcessFixture(t *testing.T) {
	mode := os.Getenv("FEISHU_ACTION_TEST_PROCESS")
	if mode == "" {
		return
	}
	switch mode {
	case "success":
		_, _ = os.Stdout.WriteString("action output")
		os.Exit(0)
	case "exit":
		os.Exit(17)
	case "overflow":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 1024*1024+1))
		os.Exit(0)
	case "timeout":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "descendant":
		cmd := exec.Command(os.Args[0], "-test.run=^TestActionProcessFixture$")
		cmd.Env = envList(cleanActionEnv(map[string]string{"FEISHU_ACTION_TEST_PROCESS": "heartbeat", "FEISHU_ACTION_HEARTBEAT": os.Getenv("FEISHU_ACTION_HEARTBEAT"), "GORACE": "atexit_sleep_ms=0"}))
		if err := cmd.Start(); err != nil {
			os.Exit(20)
		}
		for i := 0; i < 100; i++ {
			if data, _ := os.ReadFile(os.Getenv("FEISHU_ACTION_HEARTBEAT")); len(data) > 0 {
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(21)
	case "heartbeat":
		f, err := os.OpenFile(os.Getenv("FEISHU_ACTION_HEARTBEAT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(22)
		}
		// Bound the helper itself so a broken ownership regression cannot
		// leave a test process running indefinitely.
		for i := 0; i < 300; i++ {
			_, _ = f.WriteString(".")
			time.Sleep(10 * time.Millisecond)
		}
		_ = f.Close()
		os.Exit(0)
	default:
		os.Exit(19)
	}
}
func TestActionRunnerBoundsRealChildOutputAndTime(t *testing.T) {
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"success", "exit", "overflow", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			env := cleanActionEnv(map[string]string{"FEISHU_ACTION_TEST_PROCESS": mode, "GORACE": "atexit_sleep_ms=0"})
			timeout := int64(10000)
			if mode == "timeout" {
				timeout = 30
			}
			code, out, e := runActionCommand(context.Background(), executable, []string{"-test.run=^TestActionProcessFixture$"}, "", env, timeout)
			switch mode {
			case "success":
				if e != nil || code != 0 || out != "action output" {
					t.Fatal(code, out, e)
				}
			case "exit":
				if e != nil || code != 17 {
					t.Fatal(code, e)
				}
			case "overflow":
				if e == nil || !strings.Contains(e.Error(), "output size limit") || len(out) != 1024*1024 {
					t.Fatal("output overflow not bounded", len(out), e)
				}
			case "timeout":
				if e == nil || !strings.Contains(e.Error(), "timed out") {
					t.Fatal("timeout not enforced", e)
				}
			}
		})
	}
}

func TestUnconfirmedFactoryFailureRetainsSlotAndServiceLock(t *testing.T) {
	f := newFixture(t, nil)
	entered := make(chan struct{})
	f.g.factory = func(context.Context, core.Room, core.Session, core.Workspace, func(core.WorkerEvent)) (core.Worker, error) {
		close(entered)
		return nil, process.ErrUnconfirmed
	}
	f.send(f.message("one", "task", "member"))
	<-entered
	eventually(t, func() bool {
		s := f.g.getSlot(f.room().Key)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopUnconfirmed
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := f.g.Close(ctx); e == nil {
		t.Fatal("unconfirmed failed startup released service lock")
	}
	for _, s := range f.g.allSlots() {
		s.mu.Lock()
		s.stopUnconfirmed = false
		s.mu.Unlock()
	}
}
func TestEarlyNativeControlWaitsForFactoryHandle(t *testing.T) {
	f := newFixture(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	f.g.factory = func(ctx context.Context, r core.Room, s core.Session, workspace core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		w := &fakeWorker{emit: emit, sent: make(chan core.Delivery, 100), responses: make(chan core.Args, 100)}
		f.started <- w
		emit(core.WorkerEvent{Type: "request", ID: "early-history", Kind: "history", Input: core.Args{}})
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return w, nil
	}
	f.send(f.message("one", "task", "member"))
	<-entered
	w := f.worker()
	select {
	case a := <-w.responses:
		t.Fatal("request responded before factory published handle", a)
	default:
	}
	close(release)
	if a := response(t, w); stringArg(a, "requestID") != "early-history" {
		t.Fatal("early request lost", a)
	}
	_ = f.delivery(w)
}

func TestStopCannotClearUnknownConcurrentStartup(t *testing.T) {
	f := newFixture(t, nil)
	entered := make(chan struct{})
	f.g.factory = func(ctx context.Context, _ core.Room, _ core.Session, _ core.Workspace, _ func(core.WorkerEvent)) (core.Worker, error) {
		close(entered)
		<-ctx.Done()
		return nil, process.ErrUnconfirmed
	}
	f.send(f.message("one", "task", "member"))
	<-entered
	before, err := f.s.Session(f.room().Key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.g.Message(ctx, f.message("new", "/new", "owner")); err == nil {
		t.Fatal("new session ignored startup cleanup failure")
	}
	s := f.g.getSlot(f.room().Key)
	if s == nil {
		t.Fatal("uncertain startup released its reserved slot")
	}
	after, err := f.s.Session(f.room().Key)
	if err != nil || after.ID != before.ID {
		t.Fatal("session rotated after uncertain startup", after.ID, before.ID, err)
	}
	s.mu.Lock()
	unconfirmed := s.stopUnconfirmed
	s.stopUnconfirmed = false // The fixture creates no actual process.
	s.mu.Unlock()
	if !unconfirmed {
		t.Fatal("stop cleared unconfirmed startup ownership")
	}
}

func TestActionCompletionCleansDescendantsWithClosedStdio(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	env := cleanActionEnv(map[string]string{"FEISHU_ACTION_TEST_PROCESS": "descendant", "FEISHU_ACTION_HEARTBEAT": heartbeat, "GORACE": "atexit_sleep_ms=0"})
	code, _, err := runActionCommand(context.Background(), executable, []string{"-test.run=^TestActionProcessFixture$"}, "", env, 3000)
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil || len(before) == 0 {
		t.Fatal("descendant did not start", err)
	}
	time.Sleep(75 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil || len(after) != len(before) {
		t.Fatal("action reported completion while descendant still ran", len(before), len(after), err)
	}
}
