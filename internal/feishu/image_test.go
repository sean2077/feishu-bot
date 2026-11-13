package feishu

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUploadImage(t *testing.T) {
	db := fixtureDB(t)
	client, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/open-apis/im/v1/images" {
			t.Errorf("unexpected upload request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			w.WriteHeader(400)
			return
		}
		if got := r.FormValue("image_type"); got != "message" {
			t.Errorf("image_type = %q", got)
		}
		file, _, err := r.FormFile("image")
		if err != nil {
			t.Errorf("missing image: %v", err)
		} else {
			data, _ := io.ReadAll(file)
			_ = file.Close()
			if string(data) != "gif-bytes" {
				t.Errorf("image bytes = %q", data)
			}
		}
		apiResponse(w, map[string]string{"image_key": "img_fixture"})
	})
	key, err := client.UploadImage(context.Background(), []byte("gif-bytes"))
	if err != nil || key != "img_fixture" {
		t.Fatalf("upload image = %q %v", key, err)
	}
	if _, err = client.UploadImage(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "invalid image size") {
		t.Fatalf("empty image error = %v", err)
	}
}
