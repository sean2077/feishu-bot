package core

import (
	"strings"
	"testing"
)

func TestValidModelIdentifier(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "deepseek-v4.1-flash-1017[1m]", "provider/model:beta"} {
		if !ValidModelIdentifier(model) {
			t.Fatalf("rejected valid model %q", model)
		}
	}
	for _, model := range []string{"", "has space", "line\nbreak", "tab\tmodel", "control\x01model"} {
		if ValidModelIdentifier(model) {
			t.Fatalf("accepted invalid model %q", model)
		}
	}
	if ValidModelIdentifier(strings.Repeat("a", 201)) {
		t.Fatal("accepted an oversized model")
	}
}
