package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNonemptyFieldErrorsDistinguishEmptyAndNUL(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: "dataDir must be a nonempty string"},
		{name: "nul", value: "state\x00dir", want: "dataDir contains a NUL control character; write it as an escaped sequence in the JSON string instead of a raw character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := fixture(t)
			v["dataDir"] = tc.value
			data, _ := json.Marshal(v)
			_, err := Parse(data, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
