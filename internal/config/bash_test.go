package config

import (
	"encoding/json"
	"testing"
)

func TestBashRejectCwdPrefixConfig(t *testing.T) {
	v := fixture(t)
	projectConfig(v)["bash"] = map[string]any{"rejectCwdPrefix": true}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Projects[0].Bash.RejectCwdPrefix {
		t.Fatal("bash.rejectCwdPrefix not preserved")
	}

	v = fixture(t)
	data, _ = json.Marshal(v)
	c, err = Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].Bash.RejectCwdPrefix {
		t.Fatal("bash.rejectCwdPrefix must default to false")
	}

	v = fixture(t)
	projectConfig(v)["bash"] = map[string]any{"other": true}
	data, _ = json.Marshal(v)
	if _, err := Parse(data, t.TempDir()); err == nil {
		t.Fatal("unknown bash field accepted")
	}
}
