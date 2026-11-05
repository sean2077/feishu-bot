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
