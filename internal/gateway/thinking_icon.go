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
	g.thinkingIconMu.Lock()
	if g.thinkingIconLoaded || g.thinkingIconLoading {
		g.thinkingIconMu.Unlock()
		return
	}
	g.thinkingIconLoading = true
	g.thinkingIconMu.Unlock()
	key := ""
	if uploader, ok := g.transport.(core.ImageUploader); ok {
		if uploaded, err := uploader.UploadImage(ctx, thinkingGIF); err == nil {
			key = uploaded
		}
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
