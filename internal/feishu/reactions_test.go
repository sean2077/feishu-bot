package feishu

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestUncertainReactionWritesAreReconciledNeverRecreated(t *testing.T) {
	for _, previous := range []string{"attempt:processing", "attempt:done", "processing", "done"} {
		t.Run(previous, func(t *testing.T) {
			writes := 0
			c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				apiResponse(w, map[string]any{"items": []any{}, "has_more": false})
			})
			state := "done"
			if strings.HasSuffix(previous, "processing") {
				state = "processing"
			}
			if err := c.Receipt(context.Background(), "om_origin", state, previous); err == nil || writes != 0 {
				t.Fatal("ambiguous write recreated or confirmed absent")
			}
		})
	}
}
func TestReactionCleanupTouchesOnlyThisApplicationsReaction(t *testing.T) {
	removed := []string{}
	adds := 0
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			apiResponse(w, map[string]any{"items": []any{
				map[string]any{"reaction_id": "reaction_foreign", "operator": map[string]string{"operator_type": "app", "operator_id": "cli_other"}},
				map[string]any{"reaction_id": "reaction_human", "operator": map[string]string{"operator_type": "user", "operator_id": "ou_owner"}},
				map[string]any{"reaction_id": "reaction_own", "operator": map[string]string{"operator_type": "app", "operator_id": "cli_fixture"}},
			}, "has_more": false})
		case "DELETE":
			removed = append(removed, r.URL.Path)
			apiResponse(w, map[string]any{})
		case "POST":
			adds++
			apiResponse(w, map[string]string{"reaction_id": "reaction_done"})
		}
	})
	if err := c.Receipt(context.Background(), "om_origin", "done", "processing"); err != nil {
		t.Fatal(err)
	}
	if adds != 1 || len(removed) != 1 || !strings.HasSuffix(removed[0], "/reaction_own") {
		t.Fatalf("foreign reaction modified: %v %d", removed, adds)
	}
}

func TestReactionRemovalAfterRestartDistinguishesConfirmedAndUnknownCreates(t *testing.T) {
	for _, prior := range []string{"processing", "done", "attempt:processing", "attempt:done"} {
		t.Run(prior, func(t *testing.T) {
			writes := 0
			// A fresh Client has lost the reaction ID. The previous DELETE took
			// effect before its acknowledgement/local state survived the crash.
			c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				apiResponse(w, map[string]any{"items": []any{}, "has_more": false})
			})
			err := c.Receipt(context.Background(), "om_origin", "clear", prior)
			unknown := strings.HasPrefix(prior, "attempt:")
			if (err != nil) != unknown || writes != 0 {
				t.Fatalf("cleanup prior=%s err=%v writes=%d", prior, err, writes)
			}
		})
	}
}

func TestConfirmedProcessingCleanupAfterRestartCanComplete(t *testing.T) {
	adds := 0
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			apiResponse(w, map[string]any{"items": []any{}, "has_more": false})
		case "POST":
			adds++
			apiResponse(w, map[string]string{"reaction_id": "reaction_done"})
		default:
			t.Error("empty observation must not remove a foreign reaction", r.Method)
		}
	})
	if err := c.Receipt(context.Background(), "om_origin", "clear", "processing"); err != nil {
		t.Fatal("confirmed removal never converged after restart", err)
	}
	if err := c.Receipt(context.Background(), "om_origin", "done", "clear"); err != nil || adds != 1 {
		t.Fatalf("Done remained blocked: adds=%d err=%v", adds, err)
	}
}
