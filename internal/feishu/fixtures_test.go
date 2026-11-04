package feishu

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
	_ "modernc.org/sqlite"
)

func fixtureConfig() core.Config {
	return core.Config{TenantID: "fixture-tenant", AppID: "cli_fixture", AppSecretEnv: "FEISHU_FIXTURE_SECRET", Projects: []core.Project{{ID: "fixture", Maintainers: []string{"ou_owner"}}}, Groups: []core.Group{{ChatID: "oc_allowed", ProjectID: "fixture", Members: []string{"ou_member"}}}, DMs: []core.DM{{UserID: "ou_dm", ProjectID: "fixture"}}, Limits: core.Limits{AttachmentBytes: 1 << 20, TextChars: 32000}}
}
func fixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func jsonResponse(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func apiResponse(w http.ResponseWriter, data any) {
	jsonResponse(w, 200, map[string]any{"code": 0, "data": data})
}
func fixtureClient(t *testing.T, db *sql.DB, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	t.Setenv("FEISHU_FIXTURE_SECRET", "fixture-app-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			jsonResponse(w, 200, map[string]any{"code": 0, "tenant_access_token": "fixture-token", "expire": 7200})
			return
		}
		if r.URL.Path == "/open-apis/bot/v3/info" {
			jsonResponse(w, 200, map[string]any{"code": 0, "bot": map[string]string{"open_id": "ou_bot"}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/open-apis/") && r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected authorization header")
			w.WriteHeader(401)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := New(fixtureConfig(), db, nil, Options{BaseURL: server.URL, HTTPClient: server.Client(), BotOpenID: "ou_bot"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, server
}
func fixtureRoom(t *testing.T, c *Client) core.Room {
	t.Helper()
	room, ok := roomFor(c.config, "oc_allowed", "")
	if !ok {
		t.Fatal("fixture room missing")
	}
	return room
}
func rawEvent(chat, user, kind, content string, mentioned bool) []byte {
	mentions := []any{}
	if mentioned {
		mentions = append(mentions, map[string]any{"key": "@_user_1", "id": map[string]string{"open_id": "ou_bot"}, "name": "Fixture Bot"})
	}
	value := map[string]any{"schema": "2.0", "header": map[string]string{"event_type": "im.message.receive_v1", "event_id": "evt_fixture"}, "event": map[string]any{"sender": map[string]any{"sender_id": map[string]string{"open_id": user}, "sender_type": "user"}, "message": map[string]any{"message_id": "om_input", "chat_id": chat, "chat_type": "group", "message_type": kind, "content": content, "create_time": "1700000000123", "parent_id": "om_parent", "root_id": "om_root", "thread_id": "omt_thread", "mentions": mentions}}}
	b, _ := json.Marshal(value)
	return b
}
func fixtureItem(id, chat, sender, kind, content string) map[string]any {
	return map[string]any{"message_id": id, "chat_id": chat, "msg_type": kind, "create_time": "1700000000123", "sender": map[string]string{"id": sender, "id_type": "open_id", "sender_type": "user"}, "body": map[string]string{"content": content}}
}
func decodedBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		t.Error("invalid request JSON")
	}
	return body
}
func ignoreMessage(context.Context, core.Message) error { return nil }
