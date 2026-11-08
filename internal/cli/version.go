package cli

import "runtime/debug"

// Version is injected with -ldflags '-X github.com/sean2077/feishu-bot/internal/cli.Version=...'.
// Releases inject the tag without "v" (X.Y.Z); local stamped builds inject
// `git describe --tags --long --dirty` without the leading "v". The default
// never names a release, so an unstamped build cannot look like one.
var Version = developmentVersion

const developmentVersion = "dev"

// buildVersion reports the injected version. An unstamped build falls back to
// the revision recorded by the Go toolchain, in git describe's suffix form.
func buildVersion() string {
	info, _ := debug.ReadBuildInfo()
	var settings []debug.BuildSetting
	if info != nil {
		settings = info.Settings
	}
	return describeBuild(Version, settings)
}

func describeBuild(version string, settings []debug.BuildSetting) string {
	if version != developmentVersion {
		return version
	}
	revision, modified := "", false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if len(revision) < 7 {
		return version
	}
	version += "-g" + revision[:7]
	if modified {
		version += "-dirty"
	}
	return version
}
