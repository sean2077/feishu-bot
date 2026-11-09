package config

import (
	"encoding/json"
	"testing"
)

func TestFeedbackConfig(t *testing.T) {
	v := fixture(t)
	projectConfig(v)["feedback"] = map[string]any{"enabled": true}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Projects[0].Feedback.Enabled {
		t.Fatal("feedback.enabled was not preserved")
	}
	v = fixture(t)
	data, _ = json.Marshal(v)
	c, err = Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].Feedback.Enabled {
		t.Fatal("feedback.enabled must default to false")
	}
}

func TestFeedbackConfigRejectsInvalidValues(t *testing.T) {
	for name, feedback := range map[string]any{
		"wrong type":   map[string]any{"enabled": "true"},
		"unknown key":  map[string]any{"other": true},
		"null object":  nil,
		"null enabled": map[string]any{"enabled": nil},
	} {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			projectConfig(v)["feedback"] = feedback
			data, _ := json.Marshal(v)
			if _, err := Parse(data, t.TempDir()); err == nil {
				t.Fatal("accepted invalid feedback configuration")
			}
		})
	}
}
