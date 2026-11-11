package config

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func parseHistoryChats(t *testing.T, value any) (core.Config, error) {
	t.Helper()
	v := fixture(t)
	dm := v["dms"].([]any)[0].(map[string]any)
	dm["historyChats"] = value
	data, _ := json.Marshal(v)
	return Parse(data, t.TempDir())
}

func TestHistoryChatsDMConfiguration(t *testing.T) {
	c, err := parseHistoryChats(t, []any{map[string]any{"chatId": "oc_example_group", "name": "example group"}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.HistoryChatTarget("ou_example_owner", "example group")
	if !ok || got.ChatID != "oc_example_group" || got.Name != "example group" {
		t.Fatalf("resolved history chat = %+v %v", got, ok)
	}
	room, target, ok := c.HistoryChatRoom("ou_example_owner", "oc_example_group")
	if !ok || target.Name != "example group" || room.ChatID != "oc_example_group" || room.DMUser != "" {
		t.Fatalf("resolved history room = %+v %+v %v", room, target, ok)
	}
	if _, ok := c.HistoryChatTarget("ou_example_owner", "missing"); ok {
		t.Fatal("unknown target resolved")
	}
	if _, _, ok := c.HistoryChatRoom("ou_example_owner", "*"); ok {
		t.Fatal("wildcard target resolved")
	}
}

func TestHistoryChatsRejectsInvalidTargets(t *testing.T) {
	cases := map[string]any{
		"unknown target":    []any{map[string]any{"chatId": "oc_missing_group", "name": "missing"}},
		"wildcard target":   []any{map[string]any{"chatId": "*", "name": "wildcard"}},
		"empty name":        []any{map[string]any{"chatId": "oc_example_group", "name": " "}},
		"control name":      []any{map[string]any{"chatId": "oc_example_group", "name": "bad\nname"}},
		"duplicate chat":    []any{map[string]any{"chatId": "oc_example_group", "name": "one"}, map[string]any{"chatId": "oc_example_group", "name": "two"}},
		"duplicate name":    []any{map[string]any{"chatId": "oc_example_group", "name": "same"}, map[string]any{"chatId": "oc_other_group", "name": "same"}},
		"name matches chat": []any{map[string]any{"chatId": "oc_example_group", "name": "oc_other_group"}, map[string]any{"chatId": "oc_other_group", "name": "other"}},
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			groups := v["groups"].([]any)
			other := map[string]any{"chatId": "oc_other_group", "projectId": "assistant", "members": []any{"ou_example_member"}}
			v["groups"] = append(groups, other)
			dm := v["dms"].([]any)[0].(map[string]any)
			dm["historyChats"] = value
			data, _ := json.Marshal(v)
			if _, err := Parse(data, t.TempDir()); err == nil {
				t.Fatal("invalid historyChats accepted")
			}
		})
	}
}
