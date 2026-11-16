package config

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func parseAutoCompactWindow(t *testing.T, value any) (core.Config, error) {
	t.Helper()
	v := fixture(t)
	project := v["projects"].([]any)[0].(map[string]any)
	project["autoCompactWindow"] = value
	data, _ := json.Marshal(v)
	return Parse(data, t.TempDir())
}

func TestProjectAutoCompactWindow(t *testing.T) {
	for _, value := range []int64{100000, 400000, 1000000} {
		c, err := parseAutoCompactWindow(t, value)
		if err != nil {
			t.Fatalf("%d: %v", value, err)
		}
		if c.Projects[0].AutoCompactWindow != value {
			t.Fatalf("%d parsed as %d", value, c.Projects[0].AutoCompactWindow)
		}
	}
	for _, value := range []int64{-1, 99999, 1000001} {
		if _, err := parseAutoCompactWindow(t, value); err == nil {
			t.Fatalf("%d was accepted", value)
		}
	}
	v := fixture(t)
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].AutoCompactWindow != 0 {
		t.Fatalf("absent autoCompactWindow = %d", c.Projects[0].AutoCompactWindow)
	}
}
