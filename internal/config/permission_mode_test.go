package config

import (
	"encoding/json"
	"testing"
)

func TestPermissionModeAcceptsAuto(t *testing.T) {
	v := fixture(t)
	projectConfig(v)["permissionMode"] = "auto"
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].PermissionMode != "auto" {
		t.Fatalf("permissionMode = %q", c.Projects[0].PermissionMode)
	}
	projectConfig(v)["permissionMode"] = "auto "
	data, _ = json.Marshal(v)
	if _, err := Parse(data, t.TempDir()); err == nil {
		t.Fatal("permissionMode with trailing space accepted")
	}
}

func TestAutoModeServerEnvCannotBeOverridden(t *testing.T) {
	v := fixture(t)
	projectConfig(v)["env"] = map[string]any{"CLAUDE_CODE_AUTO_MODE_SERVER": "1"}
	data, _ := json.Marshal(v)
	if _, err := Parse(data, t.TempDir()); err == nil {
		t.Fatal("project env overrode gateway-managed auto classifier mode")
	}
}
