package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecutorNeverFollowsRedirectsOrRetriesWrites(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Location", "/other")
		w.WriteHeader(307)
	}))
	defer server.Close()
	_, err := request(context.Background(), "POST", server.URL, map[string]string{"Authorization": "secret"}, map[string]string{"revision": "main"})
	if err == nil || count != 1 || strings.Contains(err.Error(), "secret") {
		t.Fatal(count, err)
	}
}
func TestJenkinsValidatesArgumentsBeforeNetwork(t *testing.T) {
	t.Setenv("JENKINS_URL", "https://jenkins.example")
	for _, args := range [][]string{{}, {"--bad"}, {"main", "extra"}, {"main\nsecret"}} {
		if _, err := jenkins(context.Background(), "jenkins-build", args); err == nil {
			t.Fatal(args)
		}
	}
}
func TestEndpointRequiresExplicitLoopbackOptIn(t *testing.T) {
	t.Setenv("ACTION_TEST_ENDPOINT", "http://127.0.0.1:1234")
	t.Setenv("BOT_ACTION_TEST_HTTP", "")
	if _, err := endpoint("ACTION_TEST_ENDPOINT"); err == nil {
		t.Fatal("accepted cleartext")
	}
	t.Setenv("BOT_ACTION_TEST_HTTP", "1")
	if _, err := endpoint("ACTION_TEST_ENDPOINT"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACTION_TEST_ENDPOINT", "http://external.example")
	if _, err := endpoint("ACTION_TEST_ENDPOINT"); err == nil {
		t.Fatal("test mode accepts external cleartext")
	}
}
