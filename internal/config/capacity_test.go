package config

import (
	"encoding/json"
	"testing"
)

func TestOptionalCapacityConfigurationAndValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]any
		valid  bool
	}{
		{"enabled", map[string]any{"maxWorkers": 4, "maxWorkersPerChat": 2, "maxRssMiB": 1536, "idleMs": 900000, "idleMinMs": 60000}, true},
		{"disabled", map[string]any{"maxWorkers": 4, "maxWorkersPerChat": 0, "maxRssMiB": 0, "idleMs": 0, "idleMinMs": 0}, true},
		{"chat exceeds global", map[string]any{"maxWorkers": 2, "maxWorkersPerChat": 3}, false},
		{"negative memory", map[string]any{"maxWorkers": 4, "maxRssMiB": -1}, false},
		{"idle min exceeds max", map[string]any{"maxWorkers": 4, "idleMs": 1000, "idleMinMs": 2000}, false},
		{"idle min with timer disabled", map[string]any{"maxWorkers": 4, "idleMs": 0, "idleMinMs": 1000}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := fixture(t)
			v["runtime"] = test.values
			data, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(data, t.TempDir())
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}
