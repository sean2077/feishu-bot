package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestReplyCommandBareOnlyForGatewayBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "feishu-bot.exe")
	other := filepath.Join(dir, "other", "feishu-bot.exe")
	if err := os.MkdirAll(filepath.Dir(other), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{exe, other} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	g := &Gateway{selfExe: exe, exeLookup: func(string) (string, error) { return exe, nil }}
	if got := g.replyCommand(); got != "feishu-bot" {
		t.Fatalf("same binary = %q", got)
	}
	g.exeLookup = func(string) (string, error) { return other, nil }
	if got, want := g.replyCommand(), shellQuote(exe); got != want {
		t.Fatalf("different binary = %q, want %q", got, want)
	}
	g.exeLookup = func(string) (string, error) { return "", os.ErrNotExist }
	if got, want := g.replyCommand(), shellQuote(exe); got != want {
		t.Fatalf("missing binary = %q, want %q", got, want)
	}
	if got := (&Gateway{}).replyCommand(); got != "feishu-bot" {
		t.Fatalf("unknown binary = %q", got)
	}
}

func TestReplyConfigFlagOnlyForNonDefault(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("user home is unavailable")
	}
	if got := replyConfigFlag(filepath.Join(home, ".feishu-bot", "config.json")); got != "" {
		t.Fatalf("default config flag = %q", got)
	}
	if got := replyConfigFlag(""); got != "" {
		t.Fatalf("missing config flag = %q", got)
	}
	if got := replyConfigFlag("C:/custom dir/config.json"); got != " --config 'C:/custom dir/config.json'" {
		t.Fatalf("custom config flag = %q", got)
	}
}

func TestRelayWrapperCarriesEndpointConfig(t *testing.T) {
	f, _, _, _ := relayFixture(t)
	text := f.g.relayWrapper(core.Message{ID: "om_wrapper", Text: "hello"}, adoptEndpoint{ConfigPath: "C:/custom dir/config.json"})
	if !strings.Contains(text, "feishu-bot topic reply --message-id om_wrapper --config 'C:/custom dir/config.json' --text-file -") {
		t.Fatalf("wrapper: %s", text)
	}
}

func TestReleaseDeadHolderKeepsRefreshedEndpoint(t *testing.T) {
	f, _, native, _ := relayFixture(t)
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found {
		t.Fatalf("record: %v %v", found, err)
	}
	room, ok := f.g.roomFromKey(record.Room)
	if !ok {
		t.Fatal("room did not resolve")
	}
	if err := f.s.UpdateAdoptionHolder(native, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	unlock := f.g.adoptLocks.lock(native)
	done := make(chan bool, 1)
	go func() {
		_, released := f.g.releaseDeadHolder(native)
		done <- released
	}()
	select {
	case <-done:
		unlock()
		t.Fatal("release ran while the adoption lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	// A local attach or notify refreshes the holder and endpoint under this
	// same per-native lock while the stale sweep waits.
	if err := f.s.UpdateAdoptionHolder(native, os.Getpid(), 0); err != nil {
		unlock()
		t.Fatal(err)
	}
	f.g.setAdoptEndpoint(record.Room, adoptEndpoint{Socket: `\\.\pipe\fresh`, Native: native})
	unlock()
	if released := <-done; released {
		t.Fatal("stale sweep released a refreshed holder")
	}
	if _, ok := f.g.endpointFor(record.Room); !ok {
		t.Fatal("stale sweep removed the refreshed endpoint")
	}
	if _, held := f.g.holderFor(room, record.Session); !held {
		t.Fatal("refreshed holder is not reported as held")
	}
}
