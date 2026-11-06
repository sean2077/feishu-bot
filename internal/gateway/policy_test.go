package gateway

import (
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func policyConfig() core.Config {
	return core.Config{
		TenantID: "tenant",
		AppID:    "app",
		Projects: []core.Project{{ID: "project", Maintainers: []string{"owner"}}},
		Groups: []core.Group{
			{ChatID: "group", ProjectID: "project", Members: []string{"member"}, MentionOptional: true},
			{ChatID: "other", ProjectID: "project", Members: []string{"member"}},
		},
		DMs: []core.DM{{UserID: "member", ProjectID: "project"}},
	}
}

func TestRoomForCarriesMentionOptionalPerGroup(t *testing.T) {
	c := policyConfig()
	optional, ok := RoomFor(c, "group", "")
	if !ok || !optional.MentionOptional {
		t.Fatalf("optional group room: %+v", optional)
	}
	required, ok := RoomFor(c, "other", "")
	if !ok || required.MentionOptional {
		t.Fatalf("required group room: %+v", required)
	}
	dm, ok := RoomFor(c, "dm-chat", "member")
	if !ok || dm.MentionOptional {
		t.Fatalf("dm room: %+v", dm)
	}
}

func TestRoomKeyIgnoresMentionOptional(t *testing.T) {
	c := policyConfig()
	on, _ := RoomFor(c, "group", "")
	c.Groups[0].MentionOptional = false
	off, _ := RoomFor(c, "group", "")
	if on.Key != off.Key {
		t.Fatalf("room key changed with mention policy: %q != %q", on.Key, off.Key)
	}
}

func TestDefaultGroupBindingAppliesOnlyToUnlistedGroups(t *testing.T) {
	c := policyConfig()
	c.Groups = append(c.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "project"})
	room, ok := RoomFor(c, "oc_unlisted", "")
	if !ok || room.ChatID != "oc_unlisted" || room.ProjectID != "project" {
		t.Fatalf("default group room: %+v", room)
	}
	if !room.Authorizes("owner") || room.Authorizes("member") || room.Authorizes("stranger") || room.Authorizes("") {
		t.Fatal("default group must allow only maintainers")
	}
	if _, ok := RoomFor(c, "oc_dm_chat", "member"); !ok {
		t.Fatal("configured DM stopped resolving")
	}
	if _, ok := RoomFor(c, "oc_dm_chat", "stranger"); ok {
		t.Fatal("default group binding leaked into DMs")
	}
	explicit, ok := RoomFor(c, "group", "")
	if !ok || !explicit.MentionOptional || explicit.AllowAllMembers {
		t.Fatalf("explicit binding did not win: %+v", explicit)
	}
}

func TestExplicitGroupBindingOverridesDefaultPolicy(t *testing.T) {
	c := policyConfig()
	c.Groups = append(c.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "project", AllowAllMembers: true})
	explicit, _ := RoomFor(c, "group", "")
	if explicit.AllowAllMembers || explicit.Authorizes("stranger") || !explicit.Authorizes("member") {
		t.Fatalf("explicit binding inherited default policy: %+v", explicit)
	}
	fallback, _ := RoomFor(c, "oc_unlisted", "")
	if !fallback.AllowAllMembers || !fallback.Authorizes("stranger") {
		t.Fatalf("default policy: %+v", fallback)
	}
}

func TestRoomKeyIgnoresAllowAllMembers(t *testing.T) {
	c := policyConfig()
	c.Groups = append(c.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "project"})
	off, _ := RoomFor(c, "oc_unlisted", "")
	c.Groups[2].AllowAllMembers = true
	on, _ := RoomFor(c, "oc_unlisted", "")
	if on.Key != off.Key {
		t.Fatalf("room key changed with author policy: %q != %q", off.Key, on.Key)
	}
}

func TestTriggersHonorsMentionOptional(t *testing.T) {
	c := policyConfig()
	optional, _ := RoomFor(c, "group", "")
	required, _ := RoomFor(c, "other", "")
	dm, _ := RoomFor(c, "dm-chat", "member")
	cases := []struct {
		name string
		room core.Room
		msg  core.Message
		want bool
	}{
		{"required group without mention", required, core.Message{ChatType: "group"}, false},
		{"required group with mention", required, core.Message{ChatType: "group", Mentioned: true}, true},
		{"optional group without mention", optional, core.Message{ChatType: "group"}, true},
		{"optional group with mention", optional, core.Message{ChatType: "group", Mentioned: true}, true},
		{"dm without mention", dm, core.Message{ChatType: "p2p"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Triggers(tc.room, tc.msg); got != tc.want {
				t.Fatalf("Triggers = %v, want %v", got, tc.want)
			}
		})
	}
}
