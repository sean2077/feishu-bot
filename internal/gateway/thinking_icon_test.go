package gateway

import (
	"context"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

type noImageTransport struct{ core.Transport }

func TestThinkingIconUploadLogsStatus(t *testing.T) {
	cases := []struct {
		name       string
		transport  core.Transport
		wantStatus string
		wantReason string
	}{
		{name: "ok", transport: &fakeTransport{thinkingKey: "img_test"}, wantStatus: "ok"},
		{name: "upload failed", transport: &fakeTransport{}, wantStatus: "fallback", wantReason: "upload_failed"},
		{name: "unsupported", transport: noImageTransport{}, wantStatus: "fallback", wantReason: "unsupported_transport"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got core.Args
			logger := func(event string, detail core.Args) {
				if event == "thinking_icon_upload" {
					got = detail
				}
			}
			g := &Gateway{transport: tc.transport, logger: logger, ctx: context.Background()}
			g.ensureThinkingIcon(context.Background())
			if got == nil {
				t.Fatal("missing thinking_icon_upload log")
			}
			if stringArg(got, "status") != tc.wantStatus || stringArg(got, "reason") != tc.wantReason {
				t.Fatalf("thinking icon log = %#v", got)
			}
			if _, ok := got["image_key"]; ok {
				t.Fatalf("log leaked image key: %#v", got)
			}
		})
	}
}
