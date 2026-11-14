package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAdoptionResultJSONFieldNames(t *testing.T) {
	attach, err := json.Marshal(AttachResult{SessionName: "example-session", TopicURL: "https://example.invalid/topic"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"session_name":"example-session"`, `"topic_url":"https://example.invalid/topic"`} {
		if !strings.Contains(string(attach), want) {
			t.Fatalf("attach result JSON %s missing %s", attach, want)
		}
	}
	notify, err := json.Marshal(NotifyResult{SessionName: "example-session", TopicURL: "https://example.invalid/topic"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"session_name":"example-session"`, `"topic_url":"https://example.invalid/topic"`} {
		if !strings.Contains(string(notify), want) {
			t.Fatalf("notify result JSON %s missing %s", notify, want)
		}
	}
}
