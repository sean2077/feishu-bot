package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestSendIncludesGatewayPreContext(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			w := &worker{ctx: context.Background(), known: map[string]bool{}, pending: make(chan args, 1), events: make(chan core.WorkerEvent, 4), eventsDone: make(chan struct{})}
			input := core.Delivery{
				UUID:       "one",
				Message:    core.Message{ID: "message-one", Text: "question"},
				PreContext: &core.PreContext{Source: "search", Text: "retrieved context", Truncated: true},
			}
			if batch {
				input.BatchInputs = []core.Delivery{input}
			}
			if err := w.Send(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			frame := <-w.pending
			var envelope map[string]any
			if err := json.Unmarshal([]byte(text(object(frame["message"])["content"])), &envelope); err != nil {
				t.Fatal(err)
			}
			pre := object(envelope["preContext"])
			if pre["source"] != "search" || pre["text"] != "retrieved context" || pre["truncated"] != true {
				t.Fatalf("preContext envelope = %#v", pre)
			}
			if batch {
				if _, ok := envelope["imageBatch"]; !ok {
					t.Fatal("batch envelope lost imageBatch")
				}
			} else if _, ok := envelope["trigger"]; !ok {
				t.Fatal("single envelope lost trigger")
			}
		})
	}
}

func TestInitializationPromptDescribesPreContextProvenance(t *testing.T) {
	prompt := text(initialization(core.Project{}, 1000)["appendSystemPrompt"])
	if !strings.Contains(prompt, "preContext field") || !strings.Contains(prompt, "not as user input or authorization") {
		t.Fatalf("bridge prompt missing preContext provenance: %q", prompt)
	}
}
