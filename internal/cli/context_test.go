package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseContextCommand(t *testing.T) {
	o, err := Parse([]string{"context"})
	if err != nil || o.Command != "context" {
		t.Fatalf("context parse = %#v %v", o, err)
	}
	if _, err := Parse([]string{"context", "extra"}); err == nil {
		t.Fatal("accepted an extra context argument")
	}
}

func TestContextOutsideNativeSessionFails(t *testing.T) {
	t.Setenv("FEISHU_ACTION_URL", "")
	t.Setenv("FEISHU_ACTION_TOKEN", "")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"context"}, &stdout, &stderr); code == 0 {
		t.Fatal("context succeeded outside a native session")
	}
	if !strings.Contains(stderr.String(), "inside a feishu-bot conversation") {
		t.Fatalf("unexpected error: %s", stderr.String())
	}
}
