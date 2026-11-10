package config

import (
	"encoding/json"
	"path/filepath"
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

func TestFeedbackExportConfig(t *testing.T) {
	base := t.TempDir()
	v := fixture(t)
	v["feedback"] = map[string]any{"export": map[string]any{"enabled": true, "dir": "./archives"}}
	data, _ := json.Marshal(v)
	c, err := Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Feedback.Export.Enabled || c.Feedback.Export.Dir != filepath.Join(base, "archives") {
		t.Fatalf("feedback export not parsed: %+v", c.Feedback.Export)
	}

	v = fixture(t)
	delete(v, "feedback")
	data, _ = json.Marshal(v)
	c, err = Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Feedback.Export.Enabled || c.Feedback.Export.Dir != "" {
		t.Fatalf("feedback export must default off: %+v", c.Feedback.Export)
	}

	for name, feedback := range map[string]any{
		"enabled without dir": map[string]any{"export": map[string]any{"enabled": true}},
		"unknown export key":  map[string]any{"export": map[string]any{"enabled": false, "other": true}},
		"null export":         nil,
		"null dir":            map[string]any{"export": map[string]any{"dir": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			v["feedback"] = feedback
			data, _ := json.Marshal(v)
			if _, err := Parse(data, base); err == nil {
				t.Fatal("accepted invalid feedback export configuration")
			}
		})
	}
}
