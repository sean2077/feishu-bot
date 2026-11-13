package gateway

import (
	"context"
	_ "embed"

	"github.com/sean2077/feishu-bot/internal/core"
)

//go:embed assets/thinking.gif
var thinkingGIF []byte

// ensureThinkingIcon uploads the animated thinking GIF once per process. A
// transport without image upload, or a failed upload, leaves the status row on
// the static standard icon.
func (g *Gateway) ensureThinkingIcon(ctx context.Context) {
	g.ensureThinkingIconWithLogger(ctx, g.logger)
}

func (g *Gateway) ensureThinkingIconWithLogger(ctx context.Context, logger core.Logger) {
	g.thinkingIconMu.Lock()
	if g.thinkingIconLoaded || g.thinkingIconLoading {
		g.thinkingIconMu.Unlock()
		return
	}
	g.thinkingIconLoading = true
	g.thinkingIconMu.Unlock()
	key := ""
	detail := core.Args{"status": "ok"}
	if uploader, ok := g.transport.(core.ImageUploader); ok {
		uploaded, err := uploader.UploadImage(ctx, thinkingGIF)
		switch {
		case err != nil:
			detail = core.Args{"status": "fallback", "reason": "upload_failed"}
		case uploaded == "":
			detail = core.Args{"status": "fallback", "reason": "empty_key"}
		default:
			key = uploaded
		}
	} else {
		detail = core.Args{"status": "fallback", "reason": "unsupported_transport"}
	}
	if logger != nil {
		// Never log the image key; status and a fixed reason are enough to
		// distinguish a successful upload from the static-icon fallback.
		logger("thinking_icon_upload", detail)
	}
	g.thinkingIconMu.Lock()
	g.thinkingIconKey = key
	g.thinkingIconLoaded = true
	g.thinkingIconLoading = false
	g.thinkingIconMu.Unlock()
}

func (g *Gateway) thinkingIcon() string {
	g.thinkingIconMu.Lock()
	defer g.thinkingIconMu.Unlock()
	return g.thinkingIconKey
}
