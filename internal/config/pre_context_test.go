package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func preContextFixture(t *testing.T, mutate func(project map[string]any)) map[string]any {
	t.Helper()
	v := fixture(t)
	project := projectConfig(v)
	project["actions"] = []any{map[string]any{
		"id":          "search",
		"description": "read-only context search",
		"executable":  filepath.Join(t.TempDir(), "search"),
		"args":        []any{"search", "--limit", "5", "--", "{{question}}"},
		"readOnly":    true,
		"timeoutMs":   float64(10000),
		"parameters": map[string]any{
			"question": map[string]any{
				"description": "one user question",
				"pattern":     `^[^\x00-\x1f\x7f]+$`,
				"maxLength":   float64(500),
			},
		},
	}}
	project["preContext"] = map[string]any{"action": "search", "parameter": "question"}
	if mutate != nil {
		mutate(project)
	}
	return v
}

func TestPreContextConfigurationDefaults(t *testing.T) {
	v := preContextFixture(t, nil)
	c, err := Parse(mustJSON(t, v), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pc := c.Projects[0].PreContext
	if pc == nil || pc.Action != "search" || pc.Parameter != "question" || pc.TimeoutMS != 5000 || pc.MaxChars != 6000 {
		t.Fatalf("preContext defaults = %+v", pc)
	}
}

func TestPreContextConfigurationRejectsInvalidReferencesAndLimits(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing action": func(p map[string]any) { p["preContext"].(map[string]any)["action"] = "missing" },
		"missing parameter": func(p map[string]any) {
			p["preContext"].(map[string]any)["parameter"] = "missing"
		},
		"not read only": func(p map[string]any) {
			p["actions"].([]any)[0].(map[string]any)["readOnly"] = false
		},
		"timeout above action": func(p map[string]any) {
			p["preContext"].(map[string]any)["timeoutMs"] = float64(10001)
		},
		"timeout too low": func(p map[string]any) {
			p["preContext"].(map[string]any)["timeoutMs"] = float64(99)
		},
		"max chars too large": func(p map[string]any) {
			p["preContext"].(map[string]any)["maxChars"] = float64(20001)
		},
		"workspace file parameter": func(p map[string]any) {
			p["actions"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["question"].(map[string]any)["workspaceFile"] = true
		},
		"extra action template parameter": func(p map[string]any) {
			p["actions"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["other"] = map[string]any{"maxLength": float64(10)}
			p["actions"].([]any)[0].(map[string]any)["args"] = []any{"search", "{{other}}", "{{question}}"}
		},
		"parameter without separator": func(p map[string]any) {
			p["actions"].([]any)[0].(map[string]any)["args"] = []any{"search", "{{question}}"}
		},
		"unknown nested field": func(p map[string]any) {
			p["preContext"].(map[string]any)["other"] = true
		},
		"wrong field case": func(p map[string]any) {
			p = p["preContext"].(map[string]any)
			p["Action"] = p["action"]
			delete(p, "action")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v := preContextFixture(t, mutate)
			if _, err := Parse(mustJSON(t, v), t.TempDir()); err == nil {
				t.Fatal("invalid preContext configuration accepted")
			}
		})
	}
}

func TestPreContextConfigurationRequiresObject(t *testing.T) {
	v := preContextFixture(t, func(p map[string]any) { p["preContext"] = "search" })
	if _, err := Parse(mustJSON(t, v), t.TempDir()); err == nil {
		t.Fatal("non-object preContext accepted")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
