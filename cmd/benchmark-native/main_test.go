package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestUnconfirmedCleanupStopsIndependentSampling(t *testing.T) {
	calls := 0
	observed := collectSamples(settings{Samples: 4}, func(index int, _ settings) sample {
		calls++
		return sample{Index: index, Status: "failed", Cleanup: index == 1}
	})
	if calls != 2 || len(observed) != 2 || observed[1].Cleanup {
		t.Fatalf("continued sampling after unconfirmed cleanup: calls=%d samples=%#v", calls, observed)
	}
	// A failed turn whose entire process tree was stopped does not contaminate
	// the next independent sample, so it is still useful to collect that sample.
	calls = 0
	observed = collectSamples(settings{Samples: 3}, func(index int, _ settings) sample {
		calls++
		return sample{Index: index, Status: "failed", Cleanup: true}
	})
	if calls != 3 || len(observed) != 3 {
		t.Fatal("confirmed cleanup prevented subsequent independent observations")
	}
}

func TestModelFixtureRequiresExactLocalContract(t *testing.T) {
	server, calls := modelFixture()
	defer server.Close()
	for _, tc := range []struct {
		path, auth, model string
		status            int
	}{
		{"/v1/messages", "", "fixture-model", http.StatusUnauthorized},
		{"/v1/messages/other", "Bearer " + fixtureToken, "fixture-model", http.StatusNotFound},
		{"/v1/messages", "Bearer " + fixtureToken, "unexpected-model", http.StatusBadRequest},
		{"/v1/messages/count_tokens", "Bearer " + fixtureToken, "fixture-model", http.StatusOK},
		{"/v1/messages", "Bearer " + fixtureToken, "fixture-model", http.StatusOK},
	} {
		body, _ := json.Marshal(core.Args{"model": tc.model, "stream": true})
		req, err := http.NewRequest(http.MethodPost, server.URL+tc.path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", tc.auth)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("%s: status %d, want %d", tc.path, res.StatusCode, tc.status)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("model count includes rejected/count-token requests: %d", calls.Load())
	}
}

// Explicitly opted in by CI after installing the pinned native CLI. This runs
// the benchmark through real Gateway/native code, without any remote model or
// Feishu request. A fake result or a response from the wrong model cannot pass.
func TestRealNativeBenchmarkUsesLocalModelAndOneWarmWorker(t *testing.T) {
	path := os.Getenv("CLAUDE_NATIVE_PATH")
	if path == "" {
		t.Skip("set CLAUDE_NATIVE_PATH to run the real native benchmark contract")
	}
	t.Setenv("FEISHU_BASELINE_FIXTURE_TOKEN", fixtureToken)
	s := runSample(1, settings{Samples: 1, WarmTurns: 1, IdleMS: 100, Claude: path})
	if s.Status != "passed" || !s.Cleanup || !s.Stable || s.Workers != 1 || len(s.Turns) != 2 || s.ModelRequests < 2 {
		t.Fatalf("benchmark contract failed: status=%s cleanup=%t stable=%t workers=%d turns=%d requests=%d failures=%s", s.Status, s.Cleanup, s.Stable, s.Workers, len(s.Turns), s.ModelRequests, strings.Join(s.Failures, ","))
	}
	for _, turn := range s.Turns {
		if !turn.Success || !turn.Matched || turn.First == nil || turn.Result == nil || turn.Delivery == nil {
			t.Fatalf("incomplete benchmark turn: %#v", turn)
		}
	}
}

func TestBenchmarkMatrixCoversResourceBoundaries(t *testing.T) {
	matrix:=scenarios(settings{Scenario:"matrix",Samples:1})
	if len(matrix)!=9 || matrix[0].Sessions!=0 || matrix[2].Sessions!=4 || matrix[7].TextBytes!=100<<10 || !matrix[7].Attachments || !matrix[8].Reclaim {
		t.Fatalf("incomplete resource scenarios: %#v",matrix)
	}
	for _,size:=range []int{20<<10,100<<10} {
		reply:=benchmarkReply(size)
		if len(reply)!=size || !strings.HasPrefix(reply,"中") { t.Fatal("reply budget must measure UTF-8 bytes",len(reply),size) }
	}
}

func TestNoSessionBenchmarkDoesNotStartNativeCLI(t *testing.T) {
	s:=runSample(1,settings{Scenario:"no-session",Sessions:0,IdleMS:1,Claude:"not-a-real-executable"})
	if s.Status!="passed" || s.Workers!=0 || s.ModelRequests!=0 || len(s.Turns)!=0 || !s.Cleanup {
		t.Fatalf("idle gateway started a model or failed cleanup: %#v",s)
	}
}

func TestRealNativeLongChineseAttachmentAndOriginalSessionResume(t *testing.T) {
	path:=os.Getenv("CLAUDE_NATIVE_PATH")
	if path=="" { t.Skip("set CLAUDE_NATIVE_PATH for the long-answer/reclaim native contract") }
	t.Setenv("FEISHU_BASELINE_FIXTURE_TOKEN",fixtureToken)
	s:=runSample(1,settings{Scenario:"long-and-resume",Sessions:1,TextBytes:100<<10,Attachments:true,Reclaim:true,UpdateMS:20,IdleMS:100,Claude:path})
	if s.Status!="passed" || !s.Reclaimed || !s.Stable || s.Workers!=2 || len(s.Turns)!=2 || s.Transport.Downloads!=1 || s.Transport.Files<2 {
		t.Fatalf("long answer/attachment/resume failed: %#v",s)
	}
}
