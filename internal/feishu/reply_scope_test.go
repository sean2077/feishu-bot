package feishu

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestScopedOutputReceiptAllowsManagementReplyWithoutHistoryPermission(t *testing.T) {
	db := fixtureDB(t)
	replies, reads, downloads := 0, 0, 0
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/im/v1/messages/om_output/reply" && r.Method == http.MethodPost {
			replies++
			apiResponse(w, core.Args{"message_id": "om_management_reply"})
			return
		}
		if r.URL.Path == "/open-apis/im/v1/messages/om_output" && r.Method == http.MethodGet {
			reads++
			jsonResponse(w, 403, core.Args{"code": 99991672})
			return
		}
		downloads++
		jsonResponse(w, 403, core.Args{"code": 99991672})
	})
	room := fixtureRoom(t, c)
	other, ok := roomFor(c.config, "oc_other_dm", "ou_dm")
	if !ok {
		t.Fatal("missing other scoped room")
	}
	if _, err := db.Exec("CREATE TABLE outputs(id TEXT PRIMARY KEY,room TEXT,session TEXT,at INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO outputs VALUES('om_output',?,'session',1),('om_other_output',?,'other-session',1)", room.Key, other.Key); err != nil {
		t.Fatal(err)
	}
	id, err := c.Send(context.Background(), room, "current status", "om_output", "")
	if err != nil || id != "om_management_reply" || replies != 1 || reads != 0 {
		t.Fatalf("management reply required history permission: id=%q err=%v replies=%d reads=%d", id, err, replies, reads)
	}
	if _, err = c.Send(context.Background(), room, "forbidden cross-room reply", "om_other_output", ""); err == nil || replies != 1 || reads != 0 {
		t.Fatal("output receipt granted cross-room reply access")
	}
	if _, err = c.Fetch(context.Background(), "om_output", room); err == nil || reads != 1 {
		t.Fatal("reply output receipt bypassed history authorization")
	}
	if err = c.Download(context.Background(), "om_output", core.Resource{Kind: "file", Key: "file_untrusted"}, filepath.Join(t.TempDir(), "download.bin"), 100); err == nil || downloads != 0 || reads != 1 {
		t.Fatal("reply output receipt granted attachment origin access")
	}
	c.config.Groups = nil
	if _, err = c.Send(context.Background(), room, "revoked", "om_output", ""); err == nil || replies != 1 {
		t.Fatal("stored output bypassed current room authorization")
	}
}
