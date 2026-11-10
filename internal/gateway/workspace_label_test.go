package gateway

import "testing"

func TestWorkspaceLabelGroupAndDMDefaults(t *testing.T) {
	f := newFixture(t, nil)
	group := f.room()
	dm := group
	dm.DMUser = "ou_example"
	work := "/srv/example/team-tools"
	if got := f.g.workspaceLabel(group, group.ProjectID, work); got != group.ProjectID {
		t.Fatalf("group default = %q, want project id", got)
	}
	if got := f.g.workspaceLabel(dm, dm.ProjectID, work); got != work {
		t.Fatalf("DM default = %q, want full path", got)
	}
}

func TestWorkspaceLabelExplicitModes(t *testing.T) {
	f := newFixture(t, nil)
	group := f.room()
	dm := group
	dm.DMUser = "ou_example"
	work := "/srv/example/team-tools"
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"full", work},
		{"basename", "team-tools"},
		{"project", group.ProjectID},
		{"hidden", ""},
	} {
		f.g.config.Projects[0].DisplayWorkspace = tc.mode
		if got := f.g.workspaceLabel(group, group.ProjectID, work); got != tc.want {
			t.Fatalf("%s group label = %q, want %q", tc.mode, got, tc.want)
		}
		if got := f.g.workspaceLabel(dm, dm.ProjectID, work); got != tc.want {
			t.Fatalf("%s DM label = %q, want %q", tc.mode, got, tc.want)
		}
	}
}
