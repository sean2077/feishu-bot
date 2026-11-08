package cli

import (
	"runtime/debug"
	"testing"
)

func TestDescribeBuildKeepsInjectedVersionAndStampsUnstampedBuilds(t *testing.T) {
	revision := debug.BuildSetting{Key: "vcs.revision", Value: "1078d4dd646f41654bc15aafa0ba8d8da95664c7"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}
	clean := debug.BuildSetting{Key: "vcs.modified", Value: "false"}
	for _, tc := range []struct {
		name, version string
		settings      []debug.BuildSetting
		want          string
	}{
		{"release", "0.5.0", []debug.BuildSetting{revision, dirty}, "0.5.0"},
		{"stamped describe", "0.5.0-3-g1078d4d", []debug.BuildSetting{revision, dirty}, "0.5.0-3-g1078d4d"},
		{"unstamped clean", developmentVersion, []debug.BuildSetting{revision, clean}, "dev-g1078d4d"},
		{"unstamped dirty", developmentVersion, []debug.BuildSetting{revision, dirty}, "dev-g1078d4d-dirty"},
		{"no vcs metadata", developmentVersion, nil, "dev"},
		{"short revision", developmentVersion, []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}, "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeBuild(tc.version, tc.settings); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
