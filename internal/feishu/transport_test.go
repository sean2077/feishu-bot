package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestHistoryAndQuoteStayWithinCurrentRoomAuthorScope(t *testing.T) {
	var requests atomic.Int32
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		items := []any{fixtureItem("om_user", "oc_allowed", "ou_member", "text", `{"text":"allowed"}`), fixtureItem("om_other_chat", "oc_forbidden", "ou_member", "text", `{"text":"other chat"}`), fixtureItem("om_other_user", "oc_allowed", "ou_stranger", "text", `{"text":"other user"}`)}
		own := fixtureItem("om_self", "oc_allowed", "ou_bot", "text", `{"text":"own bot"}`)
		own["sender"] = map[string]string{"id": "ou_bot", "sender_type": "app"}
		items = append(items, own)
		other := fixtureItem("om_bot", "oc_allowed", "ou_other_bot", "text", `{"text":"other bot"}`)
		other["sender"] = map[string]string{"id": "ou_other_bot", "sender_type": "app"}
		items = append(items, other)
		apiResponse(w, map[string]any{"items": items, "has_more": true, "page_token": "cursor_fixture"})
	})
	room := fixtureRoom(t, c)
	page, err := c.History(context.Background(), room, core.HistoryQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].ID != "om_user" || !page.Messages[1].SenderIsSelf || page.NextPageToken != "cursor_fixture" {
		t.Fatalf("history scope: %+v", page)
	}
	for _, id := range []string{"om_other_chat", "om_other_user", "om_bot", "om_missing"} {
		m, err := c.Fetch(context.Background(), id, room)
		if err != nil || m != nil {
			t.Fatalf("quote escaped ownership: %s %v %+v", id, err, m)
		}
	}
	before := requests.Load()
	room.ChatID = "oc_forbidden"
	if _, err = c.Fetch(context.Background(), "om_user", room); err == nil || requests.Load() != before {
		t.Fatal("tampered room must fail before provider read")
	}
}
func TestIncompletePaginationAndThreadFiltering(t *testing.T) {
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("container_id_type") == "thread" {
			a := fixtureItem("om_wrongthread", "oc_allowed", "ou_member", "text", `{"text":"no"}`)
			a["thread_id"] = "omt_elsewhere"
			apiResponse(w, map[string]any{"items": []any{a}, "has_more": false})
			return
		}
		apiResponse(w, map[string]any{"items": []any{}, "has_more": true})
	})
	room := fixtureRoom(t, c)
	if _, err := c.History(context.Background(), room, core.HistoryQuery{Limit: 10}); err == nil {
		t.Fatal("missing cursor should not imply complete history")
	}
	page, err := c.History(context.Background(), room, core.HistoryQuery{Limit: 10, ThreadID: "omt_allowed"})
	if err != nil || len(page.Messages) != 0 {
		t.Fatal("thread query returned a different thread")
	}
}
func TestDownloadStreamsAndRemovesOversizeOrCancelledFiles(t *testing.T) {
	var mode atomic.Int32
	var resources atomic.Int32
	body := bytes.Repeat([]byte("x"), 64<<10)
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resources/file_fixture") {
			resources.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			if mode.Load() == 1 {
				_, _ = w.Write(body[:8])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			_, _ = w.Write(body)
			return
		}
		apiResponse(w, map[string]any{"items": []any{fixtureItem("om_input", "oc_allowed", "ou_member", "file", `{"file_key":"file_fixture","file_name":"data.bin"}`)}})
	})
	room := fixtureRoom(t, c)
	if _, err := c.Fetch(context.Background(), "om_input", room); err != nil {
		t.Fatal(err)
	}
	r := core.Resource{Key: "file_fixture", Kind: "file", Name: "data.bin"}
	path := filepath.Join(t.TempDir(), "oversize")
	if err := c.Download(context.Background(), "om_input", r, path, 1024); err == nil {
		t.Fatal("oversize resource accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("partial download retained")
	}
	mode.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	path = filepath.Join(t.TempDir(), "cancelled")
	if err := c.Download(ctx, "om_input", r, path, 1<<20); err == nil {
		t.Fatal("cancelled download accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled download retained")
	}
	before := resources.Load()
	r.Key = "file_unattached"
	if err := c.Download(context.Background(), "om_input", r, path, 100); err == nil || resources.Load() != before {
		t.Fatal("unassociated attachment was fetched")
	}
}
func TestDownloadOriginCannotTurnAllowlistedDMUserIntoAllowedGroup(t *testing.T) {
	var downloads atomic.Int32
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/resources/") {
			downloads.Add(1)
			_, _ = w.Write([]byte("secret"))
			return
		}
		apiResponse(w, map[string]any{"items": []any{fixtureItem("om_forbidden", "oc_unconfigured_group", "ou_dm", "file", `{"file_key":"file_fixture"}`)}})
	})
	if err := c.Download(context.Background(), "om_forbidden", core.Resource{Key: "file_fixture", Kind: "file"}, filepath.Join(t.TempDir(), "file"), 1024); err == nil || downloads.Load() != 0 {
		t.Fatal("DM authorization escaped to unrelated group")
	}
}
func TestUploadStreamAndMessageAreSingleAttempts(t *testing.T) {
	data := bytes.Repeat([]byte("bounded-upload"), 4096)
	path := filepath.Join(t.TempDir(), "result.csv")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var uploads, sends atomic.Int32
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/im/v1/files":
			uploads.Add(1)
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				return
			}
			fields := map[string]string{}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				body, _ := io.ReadAll(part)
				fields[part.FormName()] = string(body)
			}
			if fields["file"] != string(data) || fields["file_type"] != "stream" || fields["file_name"] != "report.csv" {
				t.Error("incorrect streamed multipart upload")
			}
			apiResponse(w, map[string]string{"file_key": "file_uploaded"})
		case "/open-apis/im/v1/messages":
			sends.Add(1)
			body := decodedBody(t, r)
			if body["msg_type"] != "file" || body["content"] != `{"file_key":"file_uploaded"}` {
				t.Errorf("file message: %+v", body)
			}
			jsonResponse(w, 503, map[string]any{"code": 1250000, "msg": "fixture-token fixture-app-secret sensitive-provider-body"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	_, err := c.File(context.Background(), fixtureRoom(t, c), path, "report.csv")
	if err == nil {
		t.Fatal("failed delivery accepted")
	}
	if uploads.Load() != 1 || sends.Load() != 1 {
		t.Fatal("upload/send was retried")
	}
	for _, secret := range []string{"fixture-token", "fixture-app-secret", "sensitive-provider-body"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("provider failure leaked secrets")
		}
	}
}
func TestReplyChecksOriginChatBeforeSending(t *testing.T) {
	var sent atomic.Int32
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			sent.Add(1)
			apiResponse(w, map[string]string{"message_id": "om_wrong"})
			return
		}
		apiResponse(w, map[string]any{"items": []any{fixtureItem("om_origin", "oc_other", "ou_member", "text", `{"text":"other"}`)}})
	})
	if _, err := c.Send(context.Background(), fixtureRoom(t, c), "reply", "om_origin", ""); err == nil || sent.Load() != 0 {
		t.Fatal("cross-chat reply write was allowed")
	}
}
func TestAPITokenFailureDoesNotReplayMutation(t *testing.T) {
	var writes atomic.Int32
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		jsonResponse(w, 200, map[string]any{"code": 99991663, "msg": "expired fixture-token"})
	})
	_, err := c.Send(context.Background(), fixtureRoom(t, c), "test", "", "")
	if err == nil || writes.Load() != 1 {
		t.Fatal("token failure must not retry a mutation")
	}
	if strings.Contains(err.Error(), "fixture-token") {
		t.Fatal("error exposed provider message")
	}
}
func TestNormalizedMessageSerializationOmitsRaw(t *testing.T) {
	m := normalizeMessage(messageEvent{Message: eventMessage{ID: "om_one", Type: "text", Content: `{"text":"safe"}`}}, "ou_bot", true)
	b, _ := json.Marshal(m)
	var decoded map[string]any
	_ = json.Unmarshal(b, &decoded)
	if _, ok := decoded["raw"]; ok {
		t.Fatal("raw event serialized")
	}
}

func TestReceivedOriginCanReplyWithoutHistoryPermission(t *testing.T) {
	reads, writes := 0, 0
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads++
			jsonResponse(w, 403, map[string]any{"code": 99991672})
			return
		}
		writes++
		if r.URL.Path != "/open-apis/im/v1/messages/om_input/reply" {
			t.Errorf("wrong reply origin: %s", r.URL.Path)
		}
		body := decodedBody(t, r)
		if body["reply_in_thread"] != true {
			t.Error("thread reply flag lost")
		}
		apiResponse(w, map[string]string{"message_id": "om_reply"})
	})
	c.onMessage = func(ctx context.Context, m core.Message) error {
		_, err := c.Send(ctx, fixtureRoom(t, c), "received", m.ID, m.ThreadID)
		return err
	}
	if _, err := c.HandleEvent(context.Background(), rawEvent("oc_allowed", "ou_member", "text", `{"text":"@_user_1 hello"}`, true)); err != nil {
		t.Fatal(err)
	}
	if reads != 0 || writes != 1 {
		t.Fatal("reply added a history scope requirement")
	}
}

func TestDownloadPreservesJSONFileAndExistingDestination(t *testing.T) {
	payload := []byte(`{"version":1,"data":"user-json-file"}`)
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/resources/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payload)
			return
		}
		apiResponse(w, map[string]any{"items": []any{fixtureItem("om_json", "oc_allowed", "ou_member", "file", `{"file_key":"file_json","file_name":"data.json"}`)}})
	})
	c.rememberOrigin("om_json", fixtureRoom(t, c))
	path := filepath.Join(t.TempDir(), "data.json")
	r := core.Resource{Key: "file_json", Kind: "file", Name: "data.json"}
	if err := c.Download(context.Background(), "om_json", r, path, 1024); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, payload) {
		t.Fatal("JSON attachment bytes changed")
	}
	if err := c.Download(context.Background(), "om_json", r, path, 1024); err == nil {
		t.Fatal("existing destination overwritten")
	}
	data, _ = os.ReadFile(path)
	if !bytes.Equal(data, payload) {
		t.Fatal("existing file was modified")
	}
}
