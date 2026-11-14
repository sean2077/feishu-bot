package core

import (
	"path/filepath"
	"testing"
)

func adoptedConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	c := scopeConfig()
	c.Projects[0].NativeConfig = "inherit"
	c.DMs[0].AdoptRoots = []string{root}
	return c
}

func TestAdoptedTopicRoomKeyAndRoundTrip(t *testing.T) {
	c := adoptedConfig(t)
	r, ok := c.AdoptedTopicRoom("oc_dm", "member", "om_root")
	if !ok {
		t.Fatal("adopted DM topic room was rejected")
	}
	if r.Key != `["tenant","app","project","p2p","oc_dm","member","om_root"]` || r.Thread != "om_root" {
		t.Fatalf("adopted room: %+v", r)
	}
	if r.WorkspaceKey() != `["tenant","app","project","p2p","oc_dm","member"]` {
		t.Fatalf("adopted room must share the DM workspace key: %q", r.WorkspaceKey())
	}
	chat, dm, thread, ok := ParseRoomKey(r.Key)
	if !ok || chat != "oc_dm" || dm != "member" || thread != "om_root" {
		t.Fatalf("parse adopted key: %q %q %q %v", chat, dm, thread, ok)
	}
	back, ok := c.RoomForKey(r.Key)
	if !ok || back.Key != r.Key || back.Thread != "om_root" || back.DMUser != "member" {
		t.Fatalf("round trip: %+v", back)
	}
	if base, ok := c.RoomForKey(r.ChatKey); !ok || base.Thread != "" || base.Key != r.ChatKey {
		t.Fatalf("DM chat room key must stay valid: %+v", base)
	}
}

func TestAdoptedTopicRequiresInheritedProjectAndRoot(t *testing.T) {
	c := adoptedConfig(t)
	c.Projects[0].NativeConfig = "isolated"
	if _, ok := c.AdoptedTopicRoom("oc_dm", "member", "om_root"); ok {
		t.Fatal("isolated project accepted an adopted topic")
	}
	c = adoptedConfig(t)
	c.DMs[0].AdoptRoots = nil
	if _, ok := c.AdoptedTopicRoom("oc_dm", "member", "om_root"); ok {
		t.Fatal("DM without adoptRoots accepted an adopted topic")
	}
	c = adoptedConfig(t)
	for _, root := range []string{"", "om/../x", "om root", "om_root\n"} {
		if _, ok := c.AdoptedTopicRoom("oc_dm", "member", root); ok {
			t.Fatalf("invalid root accepted: %q", root)
		}
	}
	if _, ok := c.AdoptedTopicRoom("oc_dm", "stranger", "om_root"); ok {
		t.Fatal("adopted topic accepted an unrelated DM user")
	}
}

func TestAdoptRootsRemovalInvalidatesStoredKey(t *testing.T) {
	c := adoptedConfig(t)
	r, ok := c.AdoptedTopicRoom("oc_dm", "member", "om_root")
	if !ok {
		t.Fatal("adopted room was rejected")
	}
	c.DMs[0].AdoptRoots = nil
	if _, ok := c.RoomForKey(r.Key); ok {
		t.Fatal("stored adopted key stayed valid after adoptRoots was removed")
	}
}

func TestAdoptedTopicRoomUsesOnlyDMNamespace(t *testing.T) {
	c := adoptedConfig(t)
	chatRoom, _ := c.RoomFor("oc_dm", "member")
	r, _ := c.AdoptedTopicRoom("oc_dm", "member", "om_root")
	if r.ChatKey != chatRoom.Key || r.WorkspaceKey() != chatRoom.Key {
		t.Fatalf("adopted topic changed the DM workspace namespace: %q", r.ChatKey)
	}
	group, _ := c.RoomFor("oc_chat", "")
	if group.Key == r.Key {
		t.Fatal("adopted topic reused a group room key")
	}
}

func TestAdoptWorkAllowed(t *testing.T) {
	root := t.TempDir()
	c := scopeConfig()
	c.DMs[0].AdoptRoots = []string{root}
	if !c.AdoptWorkAllowed("member", root) {
		t.Fatal("root itself must be allowed")
	}
	if !c.AdoptWorkAllowed("member", filepath.Join(root, "project", "sub")) {
		t.Fatal("descendant must be allowed")
	}
	if c.AdoptWorkAllowed("member", filepath.Dir(root)) {
		t.Fatal("ancestor must not be allowed")
	}
	if c.AdoptWorkAllowed("stranger", root) {
		t.Fatal("unrelated user must not be allowed")
	}
}
