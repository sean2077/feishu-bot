package feishu

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUploadIsNotCutShortByTheSharedClientTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow.bin")
	if err := os.WriteFile(path, []byte("slow upload body"), 0600); err != nil {
		t.Fatal(err)
	}
	c, _ := fixtureClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/im/v1/files":
			_, _ = io.Copy(io.Discard, r.Body)
			time.Sleep(300 * time.Millisecond)
			apiResponse(w, map[string]string{"file_key": "file_slow"})
		default:
			apiResponse(w, map[string]string{"message_id": "om_file"})
		}
	})
	// Shrink the shared client's total timeout below the upload's duration.
	c.http.Timeout = 100 * time.Millisecond
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	key, err := c.uploadFile(context.Background(), file, "slow.bin", "fixture_boundary", 1<<20)
	if err != nil || key != "file_slow" {
		t.Fatalf("upload cut short by the shared client timeout: key=%q err=%v", key, err)
	}
}
