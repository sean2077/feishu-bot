package gateway

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func endpointPathFor(t *testing.T, g *Gateway, native string) string {
	t.Helper()
	path, ok := g.adoptEndpointPath(native)
	if !ok {
		t.Fatal("native ID has no endpoint path")
	}
	return path
}

func TestAdoptEndpointPersistsAndRestores(t *testing.T) {
	f, _, native, _ := relayFixture(t)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	path := endpointPathFor(t, f.g, native)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("endpoint file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("endpoint file mode = %v", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored storedAdoptEndpoint
	if err = json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Socket != `\\.\pipe\fixture-inbox` || stored.Token != "secret-token-value" || stored.ConfigPath != "C:/config/config.json" {
		t.Fatalf("stored endpoint: %+v", stored)
	}

	// A restart only clears memory; the file is reloaded while the holder lives.
	f.g.clearAdoptEndpoint(record.Room)
	if _, ok := f.g.endpointFor(record.Room); ok {
		t.Fatal("memory endpoint survived a simulated restart")
	}
	f.g.restoreAdoptEndpoints()
	endpoint, ok := f.g.endpointFor(record.Room)
	if !ok || !endpoint.Restored || endpoint.Socket != stored.Socket || endpoint.Token != stored.Token || endpoint.Native != native {
		t.Fatalf("restored endpoint: %+v %v", endpoint, ok)
	}
}

func TestAdoptEndpointRestoreRejectsDeadHolder(t *testing.T) {
	f, _, native, _ := relayFixture(t)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	path := endpointPathFor(t, f.g, native)
	if err := f.s.UpdateAdoptionHolder(native, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	f.g.clearAdoptEndpoint(record.Room)
	f.g.restoreAdoptEndpoints()
	if _, ok := f.g.endpointFor(record.Room); ok {
		t.Fatal("dead holder endpoint was restored")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale endpoint file: %v", err)
	}
}

func TestAdoptEndpointReattachWithoutInboxRemovesFile(t *testing.T) {
	f, _, native, _ := relayFixture(t)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	path := endpointPathFor(t, f.g, native)
	f.g.setAdoptEndpoint(record.Room, adoptEndpoint{Native: native})
	if _, ok := f.g.endpointFor(record.Room); ok {
		t.Fatal("empty endpoint stayed registered")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("endpoint file survived an empty reattach: %v", err)
	}
}

func TestNewSessionRemovesPersistedEndpoint(t *testing.T) {
	f, _, native, result := relayFixture(t)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	path := endpointPathFor(t, f.g, native)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("endpoint file: %v", err)
	}
	f.send(core.Message{ID: "om_new_endpoint", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: result.RootMessageID, Text: "/new", Time: time.Now().UnixMilli(), Resources: []core.Resource{}})
	waitFor(t, "endpoint file removal after /new", func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	})
	if _, ok := f.g.endpointFor(record.Room); ok {
		t.Fatal("/new kept the memory endpoint")
	}
}

func TestRestoredEndpointFailureRemovesFileAndQueues(t *testing.T) {
	f, _, native, result := relayFixture(t)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	path := endpointPathFor(t, f.g, native)
	f.g.clearAdoptEndpoint(record.Room)
	f.g.restoreAdoptEndpoints()
	if endpoint, ok := f.g.endpointFor(record.Room); !ok || !endpoint.Restored {
		t.Fatalf("restored endpoint: %+v %v", endpoint, ok)
	}
	f.g.adoptSender = func(context.Context, string, string, string) error { return os.ErrClosed }
	f.send(relayMessage(f, "om_restored_fail", "hello", result.RootMessageID))
	waitFor(t, "restored endpoint removal", func() bool {
		_, ok := f.g.endpointFor(record.Room)
		return !ok
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed restored endpoint kept its file: %v", err)
	}
	if !noticeContains(f, "转递失败") {
		t.Fatal("relay failure notice missing")
	}
	if _, status, found, err := f.s.Forwarded(record.Room, "om_restored_fail"); err != nil || !found || status != "queued" {
		t.Fatalf("forwarded row after restored failure: %q %v %v", status, found, err)
	}
	waitFor(t, "queued fallback after restored failure", func() bool {
		var status string
		if err := f.s.DB.QueryRow("SELECT status FROM inbox WHERE room=? AND id=?", record.Room, "om_restored_fail").Scan(&status); err != nil {
			return false
		}
		return status == "queued"
	})
}

func TestRestoredEndpointRelays(t *testing.T) {
	f, _, native, result := relayFixture(t)
	record, _, _ := f.s.AdoptedTopicByNative(native)
	f.g.clearAdoptEndpoint(record.Room)
	f.g.restoreAdoptEndpoints()
	var socket, token string
	f.g.adoptSender = func(_ context.Context, s, tok, text string) error {
		socket, token = s, tok
		if !strings.Contains(text, "hello restored") {
			t.Fatalf("relay text: %s", text)
		}
		return nil
	}
	f.send(relayMessage(f, "om_restored_ok", "hello restored", result.RootMessageID))
	waitFor(t, "restored relay", func() bool { return socket != "" })
	if socket != `\\.\pipe\fixture-inbox` || token != "secret-token-value" {
		t.Fatalf("restored endpoint used %q %q", socket, token)
	}
	if _, status, _, _ := f.s.Forwarded(record.Room, "om_restored_ok"); status != store.InputForwarded {
		t.Fatalf("restored relay status = %q", status)
	}
}

func TestEndpointFilesStayOutOfAudit(t *testing.T) {
	f, _, native, _ := relayFixture(t)
	if _, err := os.Stat(endpointPathFor(t, f.g, native)); err != nil {
		t.Fatalf("endpoint file: %v", err)
	}
	rows, err := f.s.DB.Query("SELECT event,detail FROM audit")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var event, detail string
		if err := rows.Scan(&event, &detail); err != nil {
			t.Fatal(err)
		}
		if event == "adopt_endpoint_restored" {
			t.Fatal("restore event appeared before a restore")
		}
		if strings.Contains(detail, "secret-token-value") || strings.Contains(detail, `\\.\pipe\fixture-inbox`) {
			t.Fatalf("endpoint content leaked into audit: %s %s", event, detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
