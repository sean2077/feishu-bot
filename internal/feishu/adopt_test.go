package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func adoptedClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	client, _ := fixtureClient(t, st.DB, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	client.config.Projects[0].NativeConfig = "inherit"
	return client, st
}

func seedAdoption(t *testing.T, st *store.Store, c *core.Config, open bool) (string, core.Room) {
	t.Helper()
	work := t.TempDir()
	c.DMs[0].AdoptRoots = []string{work}
	room, ok := c.AdoptedTopicRoom("oc_dm_chat", "ou_dm", "om_root")
	if !ok {
		t.Fatal("fixture config did not produce an adopted room")
	}
	native := "native-feishu"
	session := core.NewID()
	if _, _, err := st.BeginAdoption(native, "ou_dm", work, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.BindAdoptionRoot(native, room.Key, "om_root", session, "/cfg", false, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if open {
		if err := st.OpenAdoption(native, "omt_fixture"); err != nil {
			t.Fatal(err)
		}
	}
	return room.Key, room
}

func rawP2PEvent(thread, root string) []byte {
	value := map[string]any{
		"header": map[string]string{"event_type": "im.message.receive_v1"},
		"event": map[string]any{
			"sender": map[string]any{"sender_id": map[string]string{"open_id": "ou_dm"}, "sender_type": "user"},
			"message": map[string]any{
				"message_id": "om_in", "chat_id": "oc_dm_chat", "chat_type": "p2p", "message_type": "text",
				"content": `{"text":"hello"}`, "create_time": "1700000000123", "thread_id": thread, "root_id": root,
				"mentions": []any{},
			},
		},
	}
	data, _ := json.Marshal(value)
	return data
}

func TestAdoptedDMTopicEventRoutesToItsRoom(t *testing.T) {
	client, st := adoptedClient(t)
	roomKey, room := seedAdoption(t, st, &client.config, true)
	var received core.Message
	client.onMessage = func(_ context.Context, m core.Message) error { received = m; return nil }
	if _, err := client.HandleEvent(context.Background(), rawP2PEvent("omt_fixture", "om_root")); err != nil {
		t.Fatal(err)
	}
	if received.ID != "om_in" || received.ChatType != "p2p" || received.ThreadID != "omt_fixture" || received.RootID != "om_root" {
		t.Fatalf("inbound message: %+v", received)
	}
	origin, ok := client.origins["om_in"]
	if !ok || origin.Key != roomKey || origin.AdoptedState != store.AdoptedOpen {
		t.Fatalf("origin room: %+v %v, want %q", origin, ok, roomKey)
	}
	if room.Thread != "om_root" {
		t.Fatalf("seeded room: %+v", room)
	}
}

func TestAdoptedDMTopicMismatchIsRejectedAndAudited(t *testing.T) {
	client, st := adoptedClient(t)
	seedAdoption(t, st, &client.config, true)
	var received core.Message
	client.onMessage = func(_ context.Context, m core.Message) error { received = m; return nil }
	if _, err := client.HandleEvent(context.Background(), rawP2PEvent("omt_fixture", "om_other")); err != nil {
		t.Fatal(err)
	}
	if received.ID != "" {
		t.Fatalf("mismatched message was delivered: %+v", received)
	}
	var count int
	if err := st.DB.QueryRow("SELECT count(*) FROM audit WHERE event='adopted_topic_rejected'").Scan(&count); err != nil || count == 0 {
		t.Fatalf("rejection audit: %d %v", count, err)
	}
}

func TestPendingAdoptionTopicIsAcceptedForNotice(t *testing.T) {
	client, st := adoptedClient(t)
	roomKey, _ := seedAdoption(t, st, &client.config, false)
	var received core.Message
	client.onMessage = func(_ context.Context, m core.Message) error { received = m; return nil }
	if _, err := client.HandleEvent(context.Background(), rawP2PEvent("omt_pending", "om_root")); err != nil {
		t.Fatal(err)
	}
	if received.ID != "om_in" {
		t.Fatal("pending adoption message was dropped before the gateway could explain it")
	}
	origin, ok := client.origins["om_in"]
	if !ok || origin.Key != roomKey || origin.AdoptedState != store.AdoptedPending {
		t.Fatalf("pending origin: %+v %v", origin, ok)
	}
}

func TestUnregisteredAdoptedShapeIsRejectedByValidatedRoom(t *testing.T) {
	client, _ := adoptedClient(t)
	client.config.DMs[0].AdoptRoots = []string{t.TempDir()}
	room, ok := client.config.AdoptedTopicRoom("oc_dm_chat", "ou_dm", "om_unregistered")
	if !ok {
		t.Fatal("fixture config did not produce a shape-valid room")
	}
	if _, err := client.validatedRoom(room); err == nil {
		t.Fatal("validatedRoom accepted a shape-valid key without an adoption record")
	}
}

func TestUnregisteredDMThreadKeepsLegacyDMBehavior(t *testing.T) {
	client, _ := adoptedClient(t)
	var received core.Message
	client.onMessage = func(_ context.Context, m core.Message) error { received = m; return nil }
	if _, err := client.HandleEvent(context.Background(), rawP2PEvent("omt_unknown", "om_root_unknown")); err != nil {
		t.Fatal(err)
	}
	if received.ID != "om_in" || received.ThreadID != "omt_unknown" {
		t.Fatalf("unregistered DM topic message: %+v", received)
	}
	origin, ok := client.origins["om_in"]
	if !ok || origin.DMUser != "ou_dm" || origin.Thread != "" {
		t.Fatalf("legacy DM origin: %+v %v", origin, ok)
	}
}

func TestSendUserCardUsesOpenIDAndReturnsChat(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client, _ := fixtureClient(t, st.DB, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open-apis/im/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("receive_id_type") != "open_id" {
			t.Errorf("receive_id_type = %q", r.URL.Query().Get("receive_id_type"))
		}
		body := decodedBody(t, r)
		if body["receive_id"] != "ou_dm" || body["msg_type"] != "interactive" {
			t.Errorf("body = %#v", body)
		}
		apiResponse(w, map[string]any{"message_id": "om_root_card", "chat_id": "oc_dm_chat"})
	})
	message, chat, err := client.SendUserCard(context.Background(), "ou_dm", core.Args{
		"config":   core.Args{"wide_screen_mode": true},
		"elements": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if message != "om_root_card" || chat != "oc_dm_chat" {
		t.Fatalf("send user card: %q %q", message, chat)
	}
}
