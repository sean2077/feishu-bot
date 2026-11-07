package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestThreadWorkspacesResolveRelativeToConfiguration(t *testing.T) {
	v := fixture(t)
	v["groups"] = []any{map[string]any{
		"chatId": "oc_topics", "projectId": "assistant", "sessionScope": "thread",
		"threadWorkspaces": map[string]any{"om_feature_a": "worktrees/feature-a", "om_feature_b": "../feature-b"},
	}}
	data, _ := json.Marshal(v)
	base := t.TempDir()
	c, err := Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Project("assistant")
	for root, want := range map[string]string{"om_feature_a": filepath.Join(base, "worktrees", "feature-a"), "om_feature_b": filepath.Join(base, "..", "feature-b")} {
		r, ok := c.ThreadRoomFor("oc_topics", root)
		if !ok || r.WorkingDirectory(p) != want {
			t.Fatalf("topic %s has directory %q, want %q", root, r.WorkingDirectory(p), want)
		}
		back, ok := c.RoomForKey(r.Key)
		if !ok || back.WorkspaceCWD != r.WorkspaceCWD {
			t.Fatal("persisted topic lost its workspace binding")
		}
	}
	other, _ := c.ThreadRoomFor("oc_topics", "om_unbound")
	if other.WorkingDirectory(p) != p.CWD {
		t.Fatal("unbound topic did not retain the project directory")
	}
}

func TestThreadWorkspacesRejectInvalidPolicyAndValues(t *testing.T) {
	for _, tc := range []struct {
		name, chat, scope string
		bindings          any
	}{
		{"chat scope", "oc_topics", "chat", map[string]any{"om_root": "work"}},
		{"missing scope", "oc_topics", "", map[string]any{"om_root": "work"}},
		{"wildcard group", core.AnyGroupChatID, "thread", map[string]any{"om_root": "work"}},
		{"invalid root", "oc_topics", "thread", map[string]any{"om/other": "work"}},
		{"dot in root", "oc_topics", "thread", map[string]any{"om.root": "work"}},
		{"long root", "oc_topics", "thread", map[string]any{strings.Repeat("a", 129): "work"}},
		{"empty path", "oc_topics", "thread", map[string]any{"om_root": ""}},
		{"blank path", "oc_topics", "thread", map[string]any{"om_root": "  "}},
		{"non-string path", "oc_topics", "thread", map[string]any{"om_root": 1}},
		{"null path", "oc_topics", "thread", map[string]any{"om_root": nil}},
		{"null map", "oc_topics", "thread", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := fixture(t)
			v["groups"] = []any{map[string]any{"chatId": tc.chat, "projectId": "assistant", "sessionScope": tc.scope, "threadWorkspaces": tc.bindings}}
			data, _ := json.Marshal(v)
			if _, err := Parse(data, t.TempDir()); err == nil {
				t.Fatal("invalid topic workspace configuration accepted")
			}
		})
	}
}
