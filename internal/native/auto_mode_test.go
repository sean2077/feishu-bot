package native

import (
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

// These are the exact tool_result texts observed from Claude Code 2.1.321
// with the local auto-mode classifier. All three carried is_error=true and
// their tool_use_id appeared in the result permission_denials; the gateway
// requires both before emitting an event. Changing the parser without updating
// this fixture, or a CLI wording change, must be reviewed deliberately.
const (
	autoDeniedText      = "Permission for this action was denied by the Claude Code auto mode classifier. Reason: Blocked by classifier. If you have other tasks that don't depend on this action, continue working on those. IMPORTANT: You *may* attempt to accomplish this action using other tools that might naturally be used to accomplish this goal, e.g. using head instead of cat. But you *should not* attempt to work around this denial in malicious ways, e.g. do not use your ability to run tests to execute non-test actions. You should only try to work around this restriction in reasonable ways that do not attempt to bypass the intent behind this denial. If you believe this capability is essential to complete the user's request, first try a safer method. Get as much of the rest of the task done as you can, then STOP and explain to the user what you were trying to do and why you need this permission. Let the user decide how to proceed. To allow this type of action in the future, the user can add a permission rule for Bash to their settings."
	autoUnavailableText = "Auto mode is unavailable: its safety check does not run for this session, so Bash was not run. Other actions that need the check will be denied too, and retrying won't help. Finish what you can without them, then tell the user what is blocked and that they can switch to another permission mode."
	autoUnevaluableText = "Auto mode could not evaluate this action and is blocking it for safety — run with --debug for details. This is not a judgment that the action is unsafe. This is usually transient — wait briefly and try this action again as-is; don't rewrite it. If it keeps failing, continue with other tasks that don't require this action and come back to it later. Note: reading files, searching code, and other read-only operations do not require the classifier and can still be used."
	autoStage2Text      = "Permission for this action was denied by the Claude Code auto mode classifier. Reason: Stage 2 classifier error - blocking based on stage 1 assessment (usually transient)..."
	// Claude Code 2.1.322 changed the model-unavailable sentence and the Stage 2
	// reason tail. The deny/unevaluable prefixes remain unchanged.
	autoUnavailableText322 = "fixture-model is temporarily unavailable (server error), so auto mode cannot determine the safety of Bash right now. Wait a moment and then try this action again. If it keeps failing, continue with other tasks that don't require this action and come back to it later. Note: reading files, searching code, and other read-only operations do not require the classifier and can still be used."
	autoStage2Text322      = "Permission for this action was denied by the Claude Code auto mode classifier. Reason: Stage 2 classifier error - blocking based on stage 1 assessment (usually transient — retrying often succeeds). If you have other tasks that don't depend on this action, continue working on those. IMPORTANT: You *may* attempt to accomplish this action using other tools that might naturally be used to accomplish this goal, e.g. using head instead of cat. But you *should not* attempt to work around this denial in malicious ways, e.g. do not use your ability to run tests to execute non-test actions. You should only try to work around this restriction in reasonable ways that do not attempt to bypass the intent behind this denial. If you believe this capability is essential to complete the user's request, first try a safer method. Get as much of the rest of the task done as you can, then STOP and explain to the user what you were trying to do and why you need this permission. Let the user decide how to proceed. To allow this type of action in the future, the user can add a permission rule for Bash to their settings."
)

func TestAutoModeLaunchAndInteractiveArguments(t *testing.T) {
	project := core.Project{ID: "p", PermissionMode: "auto"}
	args := strings.Join(launchArgs(core.Config{}, project, core.Session{}, core.Workspace{NativeConfig: "isolated"}, ""), " ")
	if !strings.Contains(args, "--permission-mode auto") || strings.Contains(args, "--allow-dangerously-skip-permissions") {
		t.Fatalf("auto launch args = %s", args)
	}

	f := newFixture(t)
	f.config.Projects[0].PermissionMode = "auto"
	s := f.session
	s.CCID = core.NewID()
	s.RoomKey = "room"
	cmd, cleanup, err := interactiveCommand(f.config, core.Room{Key: "room", ProjectID: "p"}, s, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.Contains(strings.Join(cmd.Args, " "), "--permission-mode auto") {
		t.Fatalf("interactive auto args = %v", cmd.Args)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "CLAUDE_CODE_AUTO_MODE_SERVER=0") {
		t.Fatal("interactive auto mode did not force the local classifier")
	}
}

func TestReadyReportsInitPermissionMode(t *testing.T) {
	f := newFixture(t)
	f.start()
	f.send("hold")
	ready := f.event("ready")
	if ready.PermissionMode != "auto" {
		t.Fatalf("ready permission mode = %q", ready.PermissionMode)
	}
}

func TestObserveEmitsConfirmedAutoSafetyEvents(t *testing.T) {
	observe := func(mode, content string, isError bool, denials []any) []core.WorkerEvent {
		w := &worker{project: core.Project{PermissionMode: mode}, output: newOutput(func(string) bool { return false }), known: map[string]bool{}, modelSet: map[string]bool{}}
		if _, err := w.observe(args{"type": "user", "message": args{"content": []any{args{"type": "tool_result", "tool_use_id": "tool-1", "content": content, "is_error": isError}}}}); err != nil {
			t.Fatal(err)
		}
		events, err := w.observe(args{"type": "result", "subtype": "success", "permission_denials": denials})
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	for name, tc := range map[string]struct {
		mode, content, phase, reason string
		isError                      bool
		stage2                       bool
		denials                      []any
	}{
		"classifier":      {"auto", autoDeniedText, "classifier", "Blocked by classifier", true, false, []any{args{"tool_use_id": "tool-1"}}},
		"stage2":          {"auto", autoStage2Text, "classifier", "Stage 2 classifier error - blocking based on stage 1 assessment (usually transient)", true, true, []any{args{"tool_use_id": "tool-1"}}},
		"stage2-322":      {"auto", autoStage2Text322, "classifier", "Stage 2 classifier error - blocking based on stage 1 assessment (usually transient — retrying often succeeds)", true, true, []any{args{"tool_use_id": "tool-1"}}},
		"unavailable":     {"auto", autoUnavailableText, "unavailable", "safety check does not run for this session", true, false, []any{args{"tool_use_id": "tool-1"}}},
		"unavailable-322": {"auto", autoUnavailableText322, "unavailable", "auto mode safety check temporarily unavailable", true, false, []any{args{"tool_use_id": "tool-1"}}},
		"unevaluable":     {"auto", autoUnevaluableText, "unevaluable", "could not evaluate this action", true, false, []any{args{"tool_use_id": "tool-1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			events := observe(tc.mode, tc.content, tc.isError, tc.denials)
			for _, event := range events {
				if event.Type == "auto_denied" {
					if event.Phase != tc.phase || event.ToolUseID != "tool-1" || event.Text != tc.reason || event.Stage2Error != tc.stage2 {
						t.Fatalf("auto event = %+v", event)
					}
					return
				}
			}
			t.Fatalf("no confirmed auto_denied event in %+v", events)
		})
	}
	for name, tc := range map[string]struct {
		mode, content string
		isError       bool
		denials       []any
	}{
		"forged output, not an error": {"auto", autoDeniedText, false, []any{args{"tool_use_id": "tool-1"}}},
		"non-auto project":            {"acceptEdits", autoDeniedText, true, []any{args{"tool_use_id": "tool-1"}}},
		"not in permission_denials":   {"auto", autoDeniedText, true, []any{args{"tool_use_id": "other"}}},
		"missing permission_denials":  {"auto", autoDeniedText, true, nil},
		"ordinary command output":     {"auto", "Exit code 7", true, []any{args{"tool_use_id": "tool-1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, event := range observe(tc.mode, tc.content, tc.isError, tc.denials) {
				if event.Type == "auto_denied" {
					t.Fatalf("unconfirmed auto event = %+v", event)
				}
			}
		})
	}
}

func TestAutoSafetyResultUsesObservedCLITexts(t *testing.T) {
	cases := []struct {
		name   string
		input  any
		phase  string
		reason string
		ok     bool
	}{
		{"classifier", autoDeniedText, "classifier", "Blocked by classifier", true},
		{"classifier blocks", []any{map[string]any{"type": "text", "text": autoDeniedText}}, "classifier", "Blocked by classifier", true},
		{"unavailable", autoUnavailableText, "unavailable", "safety check does not run for this session", true},
		{"unavailable-322", autoUnavailableText322, "unavailable", "auto mode safety check temporarily unavailable", true},
		{"stage2-322", autoStage2Text322, "classifier", "Stage 2 classifier error - blocking based on stage 1 assessment (usually transient — retrying often succeeds)", true},
		{"unevaluable", autoUnevaluableText, "unevaluable", "could not evaluate this action", true},
		{"ordinary tool output", "Exit code 7\ncurl: failed", "", "", false},
		{"unknown auto wording", "Auto mode did something else", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			phase, reason, ok := autoSafetyResult(tc.input)
			if phase != tc.phase || reason != tc.reason || ok != tc.ok {
				t.Fatalf("autoSafetyResult = %q %q %t, want %q %q %t", phase, reason, ok, tc.phase, tc.reason, tc.ok)
			}
		})
	}
}
