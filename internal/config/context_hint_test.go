package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContextHintTokensDefaultAndValidation(t *testing.T) {
	v := fixture(t)
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].ContextHintTokens != DefaultContextHintTokens {
		t.Fatalf("omitted contextHintTokens = %d, want default %d", c.Projects[0].ContextHintTokens, DefaultContextHintTokens)
	}
	project := v["projects"].([]any)[0].(map[string]any)
	for _, test := range []struct {
		name  string
		value any
		want  int64
	}{
		{"zero", 0, DefaultContextHintTokens},
		{"minimum", 10000, 10000},
		{"configured", 123456, 123456},
		{"maximum", 5000000, 5000000},
	} {
		project["contextHintTokens"] = test.value
		data, _ = json.Marshal(v)
		c, err = Parse(data, t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if c.Projects[0].ContextHintTokens != test.want {
			t.Fatalf("%s: contextHintTokens = %d, want %d", test.name, c.Projects[0].ContextHintTokens, test.want)
		}
	}
	for _, value := range []any{-1, 9999, 5000001} {
		project["contextHintTokens"] = value
		data, _ = json.Marshal(v)
		if _, err = Parse(data, t.TempDir()); err == nil || !strings.Contains(err.Error(), "contextHintTokens") {
			t.Fatalf("contextHintTokens %v should fail: %v", value, err)
		}
	}
	project["contextHintTokens"] = "400000"
	data, _ = json.Marshal(v)
	if _, err = Parse(data, t.TempDir()); err == nil {
		t.Fatal("string contextHintTokens should fail")
	}
}
