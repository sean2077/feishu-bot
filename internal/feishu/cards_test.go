package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func replyFixture(answer string, final bool) core.Args {
	return core.Args{"schema": "2.0", "config": core.Args{"streaming_mode": !final}, "header": core.Args{"title": core.Args{"tag": "plain_text", "content": "回复"}}, "body": core.Args{"elements": []any{core.Args{"tag": "markdown", "element_id": "reply_answer", "content": answer}, core.Args{"tag": "markdown", "element_id": "reply_status", "content": "状态"}}}}
}

type observedWrite struct {
	method, path string
	body         map[string]any
}

func TestNativeCardRetainsIdentityAndSequenceAcrossRestart(t *testing.T) {
	db := fixtureDB(t)
	var mu sync.Mutex
	writes := []observedWrite{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		body := decodedBody(t, r)
		mu.Lock()
		writes = append(writes, observedWrite{r.Method, r.URL.Path, body})
		mu.Unlock()
		switch {
		case r.URL.Path == "/open-apis/cardkit/v1/cards" && r.Method == "POST":
			apiResponse(w, map[string]string{"card_id": "card_fixture"})
		case r.URL.Path == "/open-apis/im/v1/messages" && r.Method == "POST":
			apiResponse(w, map[string]string{"message_id": "om_reply"})
		default:
			apiResponse(w, map[string]any{})
		}
	}
	c, _ := fixtureClient(t, db, handler)
	id, err := c.Card(context.Background(), fixtureRoom(t, c), replyFixture("start", false), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Update(context.Background(), id, replyFixture("partial", false)); err != nil {
		t.Fatal(err)
	}
	if err = c.Update(context.Background(), id, replyFixture("partial", false)); err != nil {
		t.Fatal(err)
	}
	c2, _ := fixtureClient(t, db, handler)
	if err = c2.Update(context.Background(), id, replyFixture("final", true)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 4 {
		t.Fatalf("unexpected writes (including repeated identical update): %+v", writes)
	}
	if writes[2].path != "/open-apis/cardkit/v1/cards/card_fixture/elements/reply_answer/content" || writes[2].body["sequence"] != float64(1) || writes[2].body["uuid"] != "c_card_fixture_1" {
		t.Fatalf("stream update: %+v", writes[2])
	}
	if writes[3].path != "/open-apis/cardkit/v1/cards/card_fixture" || writes[3].body["sequence"] != float64(2) {
		t.Fatalf("final update replaced card identity: %+v", writes[3])
	}
	var seq int
	var snapshot string
	if err = db.QueryRow("SELECT sequence,snapshot FROM feishu_cards WHERE message=?", id).Scan(&seq, &snapshot); err != nil {
		t.Fatal(err)
	}
	if seq != 2 || !strings.Contains(snapshot, `"streaming_mode":false`) {
		t.Fatal("final snapshot not durable")
	}
}
func TestCardUpdateLostAcknowledgementBurnsSequenceAndDoesNotFallback(t *testing.T) {
	db := fixtureDB(t)
	var writes []observedWrite
	fail := true
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		body := decodedBody(t, r)
		writes = append(writes, observedWrite{r.Method, r.URL.Path, body})
		if r.Method == "POST" && strings.Contains(r.URL.Path, "cardkit") {
			apiResponse(w, map[string]string{"card_id": "card_one"})
			return
		}
		if r.Method == "POST" {
			apiResponse(w, map[string]string{"message_id": "om_reply"})
			return
		}
		if fail {
			fail = false
			jsonResponse(w, 503, map[string]any{"code": 1})
			return
		}
		apiResponse(w, map[string]any{})
	})
	id, err := c.Card(context.Background(), fixtureRoom(t, c), replyFixture("old", false), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Update(context.Background(), id, replyFixture("new", false)); err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	var snapshot string
	var seq int
	_ = db.QueryRow("SELECT sequence,snapshot FROM feishu_cards WHERE message=?", id).Scan(&seq, &snapshot)
	if seq != 1 || strings.Contains(snapshot, `"content":"new"`) {
		t.Fatal("uncertain snapshot was committed")
	}
	if err = c.Update(context.Background(), id, replyFixture("new", false)); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 4 || writes[3].body["sequence"] != float64(2) || writes[3].path != writes[2].path {
		t.Fatalf("unsafe retry/fallback: %+v", writes)
	}
}
func TestOnlyExplicitCardPermissionDenialUsesInlineFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, code int
		wantFallback bool
	}{{"permission-code", 200, 99991672, true}, {"forbidden", 403, 99991672, true}, {"busy", 503, 1, false}, {"unknown-code", 200, 1250000, false}} {
		t.Run(tc.name, func(t *testing.T) {
			db := fixtureDB(t)
			creates, sends := 0, 0
			c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "cardkit") {
					creates++
					jsonResponse(w, tc.status, map[string]any{"code": tc.code})
					return
				}
				sends++
				apiResponse(w, map[string]string{"message_id": "om_inline"})
			})
			id, err := c.Card(context.Background(), fixtureRoom(t, c), replyFixture("hello", false), "", "")
			if creates != 1 {
				t.Fatal("native card creation retried")
			}
			if tc.wantFallback {
				if err != nil || id != "om_inline" || sends != 1 {
					t.Fatal("permission fallback failed")
				}
			} else if err == nil || sends != 0 {
				t.Fatal("uncertain or unrelated failure created replacement card")
			}
		})
	}
}
func TestCardSendFailureDoesNotCreateReplacement(t *testing.T) {
	db := fixtureDB(t)
	requests := 0
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if strings.Contains(r.URL.Path, "cardkit") {
			apiResponse(w, map[string]string{"card_id": "card_orphan"})
			return
		}
		jsonResponse(w, 503, map[string]any{"code": 1})
	})
	if _, err := c.Card(context.Background(), fixtureRoom(t, c), replyFixture("hello", false), "", ""); err == nil || requests != 2 {
		t.Fatal("card send uncertainty was replayed")
	}
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM feishu_cards").Scan(&n)
	if n != 0 {
		t.Fatal("unacknowledged message identity was fabricated")
	}
}
func TestConcurrentCardUpdatesSerializeMonotonicSequences(t *testing.T) {
	db := fixtureDB(t)
	var mu sync.Mutex
	sequences := []int{}
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if strings.Contains(r.URL.Path, "cardkit") {
				apiResponse(w, map[string]string{"card_id": "card_one"})
			} else {
				apiResponse(w, map[string]string{"message_id": "om_reply"})
			}
			return
		}
		body := decodedBody(t, r)
		mu.Lock()
		sequences = append(sequences, int(body["sequence"].(float64)))
		mu.Unlock()
		apiResponse(w, map[string]any{})
	})
	id, err := c.Card(context.Background(), fixtureRoom(t, c), replyFixture("initial", false), "", "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(i)
			if err := c.Update(context.Background(), id, replyFixture(string(b), false)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(sequences) != 12 {
		t.Fatalf("updates missing: %v", sequences)
	}
	for i, seq := range sequences {
		if seq != i+1 {
			t.Fatalf("sequence out of order: %v", sequences)
		}
	}
	c.locks.mu.Lock()
	defer c.locks.mu.Unlock()
	if len(c.locks.items) != 0 {
		t.Fatal("per-card serialization retained idle locks")
	}
}
