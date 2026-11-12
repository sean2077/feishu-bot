package gateway

import (
	"context"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

const copyTextLimit = 30000

func (g *Gateway) copyOutput(ctx context.Context, room core.Room, p quickControl) (string, bool, error) {
	o, err := g.outputContext(ctx, p.Output)
	if err != nil || p.Output == "" || o.Room != room.Key || o.Session != p.Session || o.Kind != "reply" || o.State != "done" || !o.Success || strings.TrimSpace(o.Text) == "" {
		return "没有可复制的完整回复。", true, nil
	}
	g.task(func() {
		if err := g.sendCopyText(room, o.Text); err != nil {
			g.notify(room, "复制文本发送失败，请稍后重试。", nil)
		}
	})
	return "正在发送可复制的纯文本。", true, nil
}

func (g *Gateway) latestReplyText(room core.Room) (string, bool) {
	session, err := g.store.Session(room.Key)
	if err != nil {
		return "", false
	}
	var text string
	err = g.store.DB.QueryRow(`SELECT text FROM outbox WHERE room=? AND session=? AND kind='reply' AND state='done' AND success=1 AND text!='' ORDER BY at DESC,id DESC LIMIT 1`, room.Key, session.ID).Scan(&text)
	return text, err == nil && strings.TrimSpace(text) != ""
}

func (g *Gateway) sendCopyText(room core.Room, text string) error {
	text = core.Redact(text, g.secrets)
	if len(text) > copyTextLimit {
		text = bytePrefix(text, copyTextLimit) + "\n\n（复制内容已截断）"
	}
	content := core.Args{"zh_cn": core.Args{"content": []any{[]any{core.Args{"tag": "text", "text": text}}}}}
	_, err := g.transport.SendPost(g.ctx, room, content, "", "")
	if err != nil {
		g.audit(room.Key, "copy_delivery_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
	}
	return err
}
