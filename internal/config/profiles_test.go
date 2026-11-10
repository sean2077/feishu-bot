package config

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestProfilesUseOnlyConfiguredNativeOptions(t *testing.T) {
	v := fixture(t)
	v["modelProfiles"] = map[string]any{"careful": map[string]any{"model": "claude-opus-4-6", "effort": "high"}}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := ResolveSessionModel(c, "careful")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Model.Model != "claude-opus-4-6" || selected.Model.SmallModel != "claude-opus-4-6" || selected.Model.Effort != "high" {
		t.Fatalf("profile not resolved: %+v", selected.Model)
	}
	if c.Model.Model != "your-model-id" || c.Model.Effort != "" || selected.Model.BaseURL != c.Model.BaseURL || selected.Model.TokenEnv != c.Model.TokenEnv {
		t.Fatal("profile mutated shared defaults or changed credentials / endpoint")
	}
	if _, err := ResolveSessionModel(c, "removed-profile"); err == nil {
		t.Fatal("removed profile silently fell back to another model")
	}
}

func TestProfileValidationRejectsUnsupportedControls(t *testing.T) {
	cases := map[string]any{
		"reserved name":      map[string]any{"default": map[string]any{"model": "model"}},
		"mixed case":         map[string]any{"Careful": map[string]any{"model": "model"}},
		"unknown effort":     map[string]any{"careful": map[string]any{"model": "model", "effort": "ultracode"}},
		"empty model":        map[string]any{"careful": map[string]any{"model": ""}},
		"argument injection": map[string]any{"careful": map[string]any{"model": "--permission-mode=bypassPermissions"}},
		"endpoint override":  map[string]any{"careful": map[string]any{"model": "model", "baseUrl": "https://other.example"}},
		"null profile":       map[string]any{"careful": nil},
	}
	for name, profiles := range cases {
		t.Run(name, func(t *testing.T) {
			v := fixture(t)
			v["modelProfiles"] = profiles
			data, _ := json.Marshal(v)
			if _, err := Parse(data, t.TempDir()); err == nil {
				t.Fatal("unsupported profile accepted")
			}
		})
	}
}

func TestProfileEffortInheritanceAndExplicitAuto(t *testing.T) {
	c := core.Config{Model: core.ModelConfig{Model: "base", Effort: "high"}, ModelProfiles: map[string]core.ModelProfile{"inherit": {Model: "next"}, "automatic": {Model: "next", Effort: "auto"}}}
	inherit, _ := ResolveSessionModel(c, "inherit")
	automatic, _ := ResolveSessionModel(c, "automatic")
	if inherit.Model.Effort != "high" || automatic.Model.Effort != "auto" || c.Model.Effort != "high" {
		t.Fatal("effort precedence changed")
	}
}

func TestProjectModelProfileResolution(t *testing.T) {
	c := core.Config{Model: core.ModelConfig{Model: "base", BaseURL: "https://gateway.example/v1", TokenEnv: "TOKEN"}, ModelProfiles: map[string]core.ModelProfile{"fast": {Model: "fast-model"}, "careful": {Model: "careful-model", Effort: "high"}}, Projects: []core.Project{{ID: "p", ModelProfile: "fast"}}}
	project := c.Projects[0]
	cases := []struct {
		name, session, model, profile, source string
	}{
		{"never selected uses project default", "", "fast-model", "fast", "project"},
		{"explicit profile wins", "careful", "careful-model", "careful", "session"},
		{"explicit default pins deployment model", "default", "base", "default", "session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected, selection, err := ResolveProjectModel(c, project, tc.session)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Model.Model != tc.model || selected.Model.BaseURL != c.Model.BaseURL || selected.Model.TokenEnv != c.Model.TokenEnv {
				t.Fatalf("resolved model/credentials = %+v", selected.Model)
			}
			if selection.Profile != tc.profile || selection.Source != tc.source {
				t.Fatalf("selection = %+v", selection)
			}
			if c.Model.Model != "base" {
				t.Fatal("project resolution mutated deployment defaults")
			}
		})
	}
	noDefault := project
	noDefault.ModelProfile = ""
	selected, selection, err := ResolveProjectModel(c, noDefault, "")
	if err != nil || selected.Model.Model != "base" || selection.Profile != "default" || selection.Source != "default" {
		t.Fatalf("missing project default = %+v %+v %v", selected.Model, selection, err)
	}
	explicitDefault := project
	explicitDefault.ModelProfile = "default"
	if _, selection, err = ResolveProjectModel(c, explicitDefault, ""); err != nil || selection.Profile != "default" || selection.Source != "project" {
		t.Fatalf("explicit project default = %+v %v", selection, err)
	}
	if _, _, err = ResolveProjectModel(c, project, "removed"); err == nil {
		t.Fatal("removed session profile silently fell back to the project default")
	}
}

func TestProjectModelProfileValidation(t *testing.T) {
	v := fixture(t)
	v["modelProfiles"] = map[string]any{"fast": map[string]any{"model": "fast-model"}}
	project := v["projects"].([]any)[0].(map[string]any)
	for _, value := range []string{"fast", "default", ""} {
		t.Run("accept "+value, func(t *testing.T) {
			project["modelProfile"] = value
			data, _ := json.Marshal(v)
			if _, err := Parse(data, t.TempDir()); err != nil {
				t.Fatalf("valid project model profile rejected: %v", err)
			}
		})
	}
	project["modelProfile"] = "missing"
	data, _ := json.Marshal(v)
	if _, err := Parse(data, t.TempDir()); err == nil {
		t.Fatal("missing project model profile was accepted")
	}
}
