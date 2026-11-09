package native

import (
	"os"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func testWorkspace(t *testing.T, mode string) core.Workspace {
	t.Helper()
	return core.Workspace{Root: t.TempDir(), Home: t.TempDir(), NativeConfig: mode}
}

func TestEnvironmentProjectOverlay(t *testing.T) {
	t.Setenv("TEAM_API_TOKEN", "token-value")
	t.Setenv("HTTPS_PROXY", "http://service-proxy")
	t.Setenv("SERVICE_ONLY_VALUE", "not-for-isolated")
	w := testWorkspace(t, "isolated")
	p := core.Project{
		Env:            map[string]string{"PROFILE": "team", "HTTPS_PROXY": "http://project-proxy"},
		EnvPassthrough: []string{"TEAM_API_TOKEN"},
	}
	env := Environment(w, p, "DEPLOYMENT_APP_SECRET")
	if env["PROFILE"] != "team" {
		t.Fatalf("literal project variable missing: %#v", env["PROFILE"])
	}
	if env["TEAM_API_TOKEN"] != "token-value" {
		t.Fatalf("passthrough project variable missing: %#v", env["TEAM_API_TOKEN"])
	}
	if env["HTTPS_PROXY"] != "http://project-proxy" {
		t.Fatalf("project proxy did not override the deployment proxy: %#v", env["HTTPS_PROXY"])
	}
	if _, ok := env["SERVICE_ONLY_VALUE"]; ok {
		t.Fatal("isolated environment inherited an unrelated service variable")
	}
	if env["HOME"] != w.Home || env["USERPROFILE"] != w.Home {
		t.Fatalf("deployment home boundary changed: %#v %#v", env["HOME"], env["USERPROFILE"])
	}
}

func TestEnvironmentProjectOverlayOverridesInheritedValues(t *testing.T) {
	t.Setenv("PROFILE", "developer")
	t.Setenv("TEAM_API_TOKEN", "token-value")
	w := testWorkspace(t, "inherit")
	p := core.Project{Env: map[string]string{"PROFILE": "team"}, EnvPassthrough: []string{"TEAM_API_TOKEN"}}
	env := Environment(w, p, "DEPLOYMENT_APP_SECRET")
	if env["PROFILE"] != "team" {
		t.Fatalf("inherit mode did not apply the project value: %#v", env["PROFILE"])
	}
	if env["TEAM_API_TOKEN"] != "token-value" {
		t.Fatalf("inherit mode lost the passthrough value: %#v", env["TEAM_API_TOKEN"])
	}
}

func TestEnvironmentPassthroughMissingOrEmptyIsSkipped(t *testing.T) {
	t.Setenv("EMPTY_PASSTHROUGH", "")
	const absent = "ABSENT_PASSTHROUGH_FOR_TEST"
	previous, existed := os.LookupEnv(absent)
	if err := os.Unsetenv(absent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(absent, previous)
		} else {
			_ = os.Unsetenv(absent)
		}
	})
	w := testWorkspace(t, "isolated")
	p := core.Project{EnvPassthrough: []string{"EMPTY_PASSTHROUGH", absent}}
	env := Environment(w, p, "DEPLOYMENT_APP_SECRET")
	for _, name := range []string{"EMPTY_PASSTHROUGH", absent} {
		if _, ok := env[name]; ok {
			t.Fatalf("missing or empty passthrough was injected: %s", name)
		}
	}
}

func TestApplyProjectEnvironmentFoldsCaseWhenRequested(t *testing.T) {
	p := core.Project{Env: map[string]string{"PATH": "project-path", "PROFILE": "team"}}
	folded := map[string]string{"Path": "service-path", "profile": "developer"}
	applyProjectEnvironment(folded, p, true)
	if _, ok := folded["Path"]; ok {
		t.Fatalf("Windows case variant survived: %#v", folded)
	}
	if _, ok := folded["profile"]; ok {
		t.Fatalf("Windows case variant survived: %#v", folded)
	}
	if folded["PATH"] != "project-path" || folded["PROFILE"] != "team" {
		t.Fatalf("project values missing: %#v", folded)
	}
	exact := map[string]string{"Path": "service-path", "profile": "developer"}
	applyProjectEnvironment(exact, p, false)
	if exact["Path"] != "service-path" || exact["profile"] != "developer" {
		t.Fatalf("case-sensitive environment changed: %#v", exact)
	}
}

func TestApplyProjectEnvironmentFoldsPassthroughCase(t *testing.T) {
	t.Setenv("TEAM_API_TOKEN", "token-value")
	p := core.Project{EnvPassthrough: []string{"TEAM_API_TOKEN"}}
	env := map[string]string{"team_api_token": "stale"}
	applyProjectEnvironment(env, p, true)
	if _, ok := env["team_api_token"]; ok {
		t.Fatalf("Windows passthrough case variant survived: %#v", env)
	}
	if env["TEAM_API_TOKEN"] != "token-value" {
		t.Fatalf("passthrough value missing: %#v", env)
	}
}
