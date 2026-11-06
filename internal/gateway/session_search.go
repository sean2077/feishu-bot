package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

const sessionSearchUsage = "用法：/sessions [名称、原生标题或 ID 关键词] [--cursor 翻页凭证] [--json]；关键词最多 128 字、8 项。"

type sessionCursor struct {
	Room    string `json:"room"`
	Query   string `json:"query"`
	Before  int64  `json:"before"`
	At      int64  `json:"at"`
	ID      string `json:"id"`
	Expires int64  `json:"expires"`
}

type sessionListPage struct {
	Query      string               `json:"query"`
	Current    string               `json:"current,omitempty"`
	Total      int                  `json:"total"`
	Items      []store.SessionEntry `json:"sessions"`
	NextCursor string               `json:"nextCursor,omitempty"`
	JSON       bool                 `json:"-"`
}

func (g *Gateway) signSessionCursor(p sessionCursor) string {
	data, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, g.historyKey[:])
	_, _ = mac.Write([]byte("sessions/v1\x00"))
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (g *Gateway) readSessionCursor(token, room string, now int64) (sessionCursor, error) {
	var p sessionCursor
	if len(token) > 4096 {
		return p, errors.New("翻页凭证过长")
	}
	body, sig, ok := strings.Cut(token, ".")
	data, err := base64.RawURLEncoding.DecodeString(body)
	if !ok || err != nil {
		return p, errors.New("翻页凭证无效")
	}
	signature, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return p, errors.New("翻页凭证无效")
	}
	mac := hmac.New(sha256.New, g.historyKey[:])
	_, _ = mac.Write([]byte("sessions/v1\x00"))
	_, _ = mac.Write(data)
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(data, &p) != nil || p.Room != room || p.Expires <= now || p.Before <= 0 || p.Before > now || p.At < 0 || p.At > p.Before || p.ID == "" || len(p.ID) > 64 {
		return sessionCursor{}, errors.New("翻页凭证无效或已过期，请重新搜索")
	}
	return p, nil
}

func (g *Gateway) sessionPage(r core.Room, arg string) (sessionListPage, error) {
	p := sessionListPage{}
	words := strings.Fields(arg)
	var query []string
	token := ""
	for i := 0; i < len(words); i++ {
		switch words[i] {
		case "--json":
			if p.JSON {
				return p, errors.New(sessionSearchUsage)
			}
			p.JSON = true
		case "--cursor":
			i++
			if token != "" || i >= len(words) {
				return p, errors.New(sessionSearchUsage)
			}
			token = words[i]
		default:
			if strings.HasPrefix(words[i], "--") {
				return p, errors.New(sessionSearchUsage)
			}
			query = append(query, words[i])
		}
	}
	p.Query = strings.Join(query, " ")
	if len(query) > 8 || utf8.RuneCountInString(p.Query) > 128 {
		return p, errors.New(sessionSearchUsage)
	}
	now := time.Now().UnixMilli()
	cursor := sessionCursor{Room: r.Key, Query: p.Query, Before: now, Expires: now + 15*time.Minute.Milliseconds()}
	if token != "" {
		var err error
		cursor, err = g.readSessionCursor(token, r.Key, now)
		if err != nil {
			return p, err
		}
		if p.Query != "" && p.Query != cursor.Query {
			return p, errors.New("搜索条件已改变，请去掉旧翻页凭证重新搜索")
		}
		p.Query = cursor.Query
	}
	ctx, cancel := context.WithTimeout(g.ctx, 2*time.Second)
	defer cancel()
	items, more, err := g.store.SearchSessions(ctx, r.Key, store.SessionQuery{Text: p.Query, Before: cursor.Before, AfterAt: cursor.At, AfterID: cursor.ID, Limit: 20})
	if err != nil {
		return p, err
	}
	p.Items = items
	if err := g.store.DB.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE room=?", r.Key).Scan(&p.Total); err != nil {
		return p, err
	}
	p.Current, err = g.currentSession(r)
	if err != nil {
		return p, err
	}
	if more {
		last := items[len(items)-1]
		cursor.At, cursor.ID = last.LastInputAt, last.ID
		p.NextCursor = g.signSessionCursor(cursor)
	}
	return p, nil
}

func sessionPageText(p sessionListPage, manage bool) string {
	if p.JSON {
		items := []core.Args{}
		for _, s := range p.Items {
			items = append(items, core.Args{"id": s.ID, "shortId": cut(s.ID, 8), "name": s.Name, "nativeTitle": s.NativeTitle, "displayName": s.DisplayName(), "profile": profileLabel(s.Profile), "state": s.State, "createdAt": s.CreatedAt, "lastInputAt": s.LastInputAt, "current": s.ID == p.Current})
		}
		return jsonPretty(core.Args{"total": p.Total, "pageLimit": 20, "query": p.Query, "current": p.Current, "sessions": items, "nextCursor": p.NextCursor})
	}
	if p.Total == 0 {
		return "尚无历史会话。发送消息后会自动建立；查看列表不会启动模型。"
	}
	lines := []string{"**本聊天会话**", fmt.Sprintf("共 %d 个；每页最多 20 个，按最后接收的人类输入排序。时间为 UTC。", p.Total)}
	if p.Query != "" {
		lines = append(lines, "搜索："+p.Query)
	}
	if len(p.Items) == 0 {
		lines = append(lines, "没有匹配结果。")
	}
	for _, s := range p.Items {
		label := "历史会话"
		if s.ID == p.Current {
			label = "当前会话"
		}
		lines = append(lines, "", label+" · "+s.DisplayName()+" · "+timeLabel(s.LastInputAt)+"\n`"+s.ID+"`")
	}
	if p.Current != "" {
		lines = append(lines, "", "当前会话 ID：`"+p.Current+"`")
	}
	if p.NextCursor != "" {
		lines = append(lines, "", "下一页：`/sessions --cursor "+p.NextCursor+"`")
	}
	lines = append(lines, "", "关键词只搜索本聊天的名称、原生标题和 ID；多个关键词需同时匹配。新输入导致排序变化时，请重新搜索刷新列表。")
	if manage {
		lines = append(lines, "恢复：/resume 人工名称、完整 ID 或唯一短 ID（至少 6 位）；原生标题可能重名，请使用结果中的 ID。")
	} else {
		lines = append(lines, "恢复历史会话请联系项目维护者。")
	}
	return strings.Join(lines, "\n")
}
