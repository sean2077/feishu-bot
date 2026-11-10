package config

import (
	"encoding/json"
	"testing"
)

func TestDisplayWorkspaceValidation(t *testing.T) {
	for _, value := range []string{"", "full", "basename", "project", "hidden"} {
		v := fixture(t)
		projectConfig(v)["displayWorkspace"] = value
		data, _ := json.Marshal(v)
		if _, err := Parse(data, t.TempDir()); err != nil {
			t.Fatalf("displayWorkspace %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"Full", "absolute", "none"} {
		v := fixture(t)
		projectConfig(v)["displayWorkspace"] = value
		data, _ := json.Marshal(v)
		if _, err := Parse(data, t.TempDir()); err == nil {
			t.Fatalf("displayWorkspace %q accepted", value)
		}
	}
}
