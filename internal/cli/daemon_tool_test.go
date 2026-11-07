package cli

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRunToolOutputHelper(t *testing.T) {
	if os.Getenv("FEISHU_TEST_TOOL_OUTPUT_HELPER") != "1" {
		return
	}
	count, err := strconv.Atoi(os.Getenv("FEISHU_TEST_TOOL_OUTPUT_BYTES"))
	if err != nil {
		os.Exit(98)
	}
	if _, err := os.Stdout.Write(bytes.Repeat([]byte("x"), count)); err != nil {
		os.Exit(99)
	}
	if os.Getenv("FEISHU_TEST_TOOL_OUTPUT_STDERR") == "1" {
		_, _ = os.Stderr.Write([]byte("x"))
	}
	os.Exit(0)
}

func TestRunToolRejectsIncompleteInspectionOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		count  int
		stderr bool
		failed bool
	}{
		{"exact-limit", 64 << 10, false, false},
		{"stdout-overflow", (64 << 10) + 1, false, true},
		{"later-stderr-overflow", 64 << 10, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FEISHU_TEST_TOOL_OUTPUT_HELPER", "1")
			t.Setenv("FEISHU_TEST_TOOL_OUTPUT_BYTES", strconv.Itoa(tt.count))
			t.Setenv("FEISHU_TEST_TOOL_OUTPUT_STDERR", "0")
			if tt.stderr {
				t.Setenv("FEISHU_TEST_TOOL_OUTPUT_STDERR", "1")
			}
			output, err := runTool(context.Background(), executable, "-test.run=^TestRunToolOutputHelper$")
			if len(output) != 64<<10 || (err != nil) != tt.failed {
				t.Fatal("inspection result did not distinguish complete output", len(output), err)
			}
			if tt.failed && !strings.Contains(err.Error(), "inspection limit") {
				t.Fatal("output limit lost its diagnostic", err)
			}
		})
	}
}
