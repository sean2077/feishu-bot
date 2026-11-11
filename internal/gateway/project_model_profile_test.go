package gateway

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func applyTestProfile(t *testing.T, f *fixture, room, session, profile string) {
	t.Helper()
	if err := f.s.RequestProfile(room, session, profile); err != nil {
		t.Fatal(err)
	}
	p, err := f.s.Preferences(room, session)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := f.s.ApplyProfile(room, session, p.Revision); err != nil || !changed {
		t.Fatalf("apply profile = %t, %v", changed, err)
	}
}

func TestStatusReportsProjectModelProfileAndSource(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"fast": {Model: "fast-model"}, "careful": {Model: "careful-model"}}
		c.Projects[0].ModelProfile = "fast"
	})
	r := f.room()
	view, err := f.g.statusView(r)
	if err != nil {
		t.Fatal(err)
	}
	if view["profile"] != "fast" || view["profileSource"] != "project" {
		t.Fatalf("status without session = %v/%v", view["profile"], view["profileSource"])
	}
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	applyTestProfile(t, f, r.Key, s.ID, "careful")
	view, err = f.g.statusView(r)
	if err != nil {
		t.Fatal(err)
	}
	if view["profile"] != "careful" || view["profileSource"] != "session" {
		t.Fatalf("status with explicit profile = %v/%v", view["profile"], view["profileSource"])
	}
	applyTestProfile(t, f, r.Key, s.ID, "default")
	view, err = f.g.statusView(r)
	if err != nil {
		t.Fatal(err)
	}
	if view["profile"] != "default" || view["profileSource"] != "session" {
		t.Fatalf("explicit default did not override project default: %v/%v", view["profile"], view["profileSource"])
	}
}

func TestProjectModelProfileIsScopedToItsProject(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"fast": {Model: "fast-model"}, "careful": {Model: "careful-model"}}
		c.Projects = []core.Project{
			{ID: "project", Maintainers: []string{"owner"}, PermissionMode: "default", ModelProfile: "fast"},
			{ID: "second", Maintainers: []string{"owner"}, PermissionMode: "default", ModelProfile: "careful"},
		}
		c.Groups = []core.Group{
			{ChatID: "group", ProjectID: "project", Members: []string{"member", "second"}},
			{ChatID: "other", ProjectID: "second", Members: []string{"member"}},
		}
		c.DMs = []core.DM{{UserID: "member", ProjectID: "project"}, {UserID: "owner", ProjectID: "project"}}
	})
	first := f.room()
	second, ok := RoomFor(f.c, "other", "")
	if !ok {
		t.Fatal("second room binding missing")
	}
	for room, want := range map[string]string{first.Key: "fast", second.Key: "careful"} {
		r := first
		if room == second.Key {
			r = second
		}
		view, err := f.g.statusView(r)
		if err != nil {
			t.Fatal(err)
		}
		if view["profile"] != want || view["profileSource"] != "project" {
			t.Fatalf("room %s profile = %v/%v, want %s/project", room, view["profile"], view["profileSource"], want)
		}
	}
	s, err := f.s.Session(second.Key)
	if err != nil {
		t.Fatal(err)
	}
	applyTestProfile(t, f, second.Key, s.ID, "fast")
	view, err := f.g.statusView(second)
	if err != nil {
		t.Fatal(err)
	}
	if view["profile"] != "fast" || view["profileSource"] != "session" {
		t.Fatalf("session selection did not override that room's project default: %v/%v", view["profile"], view["profileSource"])
	}
	other, err := f.g.statusView(first)
	if err != nil {
		t.Fatal(err)
	}
	if other["profile"] != "fast" || other["profileSource"] != "project" {
		t.Fatalf("one room's selection leaked to another: %v/%v", other["profile"], other["profileSource"])
	}
}

func TestModelCommandShowsProjectDefaultAndExplicitDefaultSticks(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"fast": {Model: "fast-model"}}
		c.Projects[0].ModelProfile = "fast"
	})
	r := f.room()
	if err := f.g.modelCommand(r, f.message("view", "/model --json", "member"), "--json"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		return len(f.tr.notices) > 0
	})
	f.tr.mu.Lock()
	notice := f.tr.notices[len(f.tr.notices)-1]
	f.tr.mu.Unlock()
	var view map[string]any
	if err := json.Unmarshal([]byte(notice), &view); err != nil {
		t.Fatal(err)
	}
	if view["current"] != "fast" || view["currentSource"] != "project" || view["projectDefault"] != "fast" || view["requested"] != "" {
		t.Fatalf("model view = %#v", view)
	}
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.modelCommand(r, f.message("set", "/model default", "owner"), "default"); err != nil {
		t.Fatal(err)
	}
	p, err := f.s.Preferences(r.Key, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !((p.RequestedProfile != nil && *p.RequestedProfile == "default") || (p.Profile == "default" && p.RequestedProfile == nil)) {
		t.Fatalf("explicit default was not persisted: %+v", p)
	}
	eventually(t, func() bool {
		p, err := f.s.Preferences(r.Key, s.ID)
		return err == nil && p.Profile == "default" && p.RequestedProfile == nil
	})
	view, err = f.g.statusView(r)
	if err != nil {
		t.Fatal(err)
	}
	if view["profile"] != "default" || view["profileSource"] != "session" {
		t.Fatalf("explicit default did not stay over the project default: %v/%v", view["profile"], view["profileSource"])
	}
}

func TestSessionListReportsEffectiveProjectProfile(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.ModelProfiles = map[string]core.ModelProfile{"fast": {Model: "fast-model"}, "careful": {Model: "careful-model"}}
		c.Projects[0].ModelProfile = "fast"
	})
	r := f.room()
	selected, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	applyTestProfile(t, f, r.Key, selected.ID, "careful")
	inherited, err := f.s.NewSession(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.g.sessionPage(r, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Sessions []struct {
			ID            string `json:"id"`
			Profile       string `json:"profile"`
			ProfileSource string `json:"profileSource"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(sessionPageText(page, false)), &view); err != nil {
		t.Fatal(err)
	}
	seen := map[string][2]string{}
	for _, item := range view.Sessions {
		seen[item.ID] = [2]string{item.Profile, item.ProfileSource}
	}
	if seen[selected.ID] != [2]string{"careful", "session"} {
		t.Fatalf("explicit session profile = %v", seen[selected.ID])
	}
	if seen[inherited.ID] != [2]string{"fast", "project"} {
		t.Fatalf("project default was not shown for an unselected session: %v", seen[inherited.ID])
	}
}
