package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseCardTitle(t *testing.T, value any) (string, error) {
	t.Helper()
	v := fixture(t)
	if value != nil {
		v["cardTitle"] = value
	}
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	return c.CardTitle, err
}

func TestCardTitleDefaultsAndNormalizes(t *testing.T) {
	if got, err := parseCardTitle(t, nil); err != nil || got != "飞书助手" {
		t.Fatalf("default cardTitle = %q %v", got, err)
	}
	if got, err := parseCardTitle(t, ""); err != nil || got != "飞书助手" {
		t.Fatalf("empty cardTitle = %q %v", got, err)
	}
	if got, err := parseCardTitle(t, "  Example Bot  "); err != nil || got != "Example Bot" {
		t.Fatalf("trimmed cardTitle = %q %v", got, err)
	}
}

func TestCardTitleRejectsInvalidValues(t *testing.T) {
	for name, value := range map[string]any{
		"whitespace": "   ",
		"too long":   strings.Repeat("x", 41),
		"control":    "bad\nname",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCardTitle(t, value); err == nil {
				t.Fatal("invalid cardTitle accepted")
			}
		})
	}
}
