package native

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBashCwdPrefixMatching(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "workspace")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	slashWork := strings.ReplaceAll(work, `\`, "/")
	backslashWork := strings.ReplaceAll(work, "/", `\`)
	matches := func(command string) bool {
		target, quoted, ok := bashCwdPrefix(command)
		return ok && bashCwdMatches(target, quoted, work, home)
	}
	cases := []struct {
		name    string
		command string
		want    bool
	}{
		{"unquoted and", "cd " + work + " && echo ok", true},
		{"quoted single", "cd '" + work + "' && echo ok", true},
		{"quoted double", `cd "` + work + `" && echo ok`, true},
		{"semicolon", "cd " + work + "; echo ok", true},
		{"leading space", "  cd " + work + " && echo ok", true},
		{"trailing slash", "cd " + work + string(filepath.Separator) + " && echo ok", true},
		{"forward slashes", "cd " + slashWork + " && echo ok", true},
		{"backslashes", "cd " + backslashWork + " && echo ok", true},
		{"home expansion", "cd ~/workspace && echo ok", true},
		{"quoted home is literal", `cd "~/workspace" && echo ok`, false},
		{"other tilde", "cd ~/other && echo ok", false},
		{"other dir", "cd " + filepath.Join(home, "other") + " && echo ok", false},
		{"subdir", "cd " + filepath.Join(work, "sub") + " && echo ok", false},
		{"no separator", "cd " + work, false},
		{"or separator", "cd " + work + " || echo ok", false},
		{"dotdot", "cd .. && echo ok", false},
		{"dash", "cd - && echo ok", false},
		{"env", "cd $WORK && echo ok", false},
		{"pushd", "pushd " + work + " && echo ok", false},
		{"second cd", "cd " + filepath.Join(home, "other") + " && cd " + work, false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct {
			name    string
			command string
			want    bool
		}{"case insensitive", "cd " + strings.ToUpper(work) + " && echo ok", true})
		volume := filepath.VolumeName(work)
		if len(volume) >= 2 {
			msys := "/" + strings.ToLower(volume[:1]) + strings.ReplaceAll(work[len(volume):], `\`, "/")
			cases = append(cases, struct {
				name    string
				command string
				want    bool
			}{"msys drive", "cd " + msys + " && echo ok", true})
		}
	} else {
		cases = append(cases, struct {
			name    string
			command string
			want    bool
		}{"case sensitive", "cd " + strings.ToUpper(work) + " && echo ok", false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches(tc.command); got != tc.want {
				t.Fatalf("matches(%q) = %t, want %t", tc.command, got, tc.want)
			}
		})
	}
}

func TestBashCwdPrefixHookDeniesAndAudits(t *testing.T) {
	f := newFixture(t)
	f.config.Projects[0].Bash.RejectCwdPrefix = true
	f.start()
	w := f.worker.(*worker)
	command := "cd " + f.workspace.Work + " && echo command-marker"
	result, err := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": args{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	output := object(result["hookSpecificOutput"])
	if output["permissionDecision"] != "deny" {
		t.Fatalf("hook result = %#v", result)
	}
	reason := text(output["permissionDecisionReason"])
	if !strings.Contains(reason, "Already in") || strings.Contains(reason, "command-marker") {
		t.Fatalf("deny reason = %q", reason)
	}
	event := f.event("bash_cwd_prefix_rejected")
	if event.Type != "bash_cwd_prefix_rejected" {
		t.Fatalf("event = %+v", event)
	}

	w.project.Bash.RejectCwdPrefix = false
	result, err = w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": args{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	if object(result["hookSpecificOutput"])["permissionDecision"] == "deny" {
		t.Fatal("disabled rejectCwdPrefix denied the command")
	}

	w.project.Bash.RejectCwdPrefix = true
	result, err = w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Write", "tool_input": args{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	if object(result["hookSpecificOutput"])["permissionDecision"] == "deny" {
		t.Fatal("non-Bash tool was denied")
	}
}

func TestBashCwdPrefixDefaultOff(t *testing.T) {
	f := newFixture(t)
	f.start()
	w := f.worker.(*worker)
	result, err := w.hook(context.Background(), "tool", args{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": args{"command": "cd " + f.workspace.Work + " && echo ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if object(result["hookSpecificOutput"])["permissionDecision"] == "deny" {
		t.Fatal("default project denied cwd prefix")
	}
}
