package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func (c *Client) sendContent(ctx context.Context, room core.Room, kind string, content any, replyTo, threadID string) (string, error) {
	room, err := c.validatedRoom(room)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", errors.New("feishu: invalid message content")
	}
	body := core.Args{"msg_type": kind, "content": string(encoded)}
	path := "/im/v1/messages"
	query := url.Values{"receive_id_type": {"chat_id"}}
	if replyTo != "" {
		id, e := escapeID(replyTo)
		if e != nil {
			return "", e
		}
		// Reply is an externally visible write: check the referenced origin remains
		// readable in exactly the target room, including after config changes.
		if e := c.checkReplyOrigin(ctx, replyTo, room); e != nil {
			return "", e
		}
		path += "/" + id + "/reply"
		query = nil
		body["reply_in_thread"] = threadID != ""
	} else {
		if threadID != "" {
			return "", errors.New("feishu: thread reply requires an origin message")
		}
		body["receive_id"] = room.ChatID
	}
	var data struct {
		ID string `json:"message_id"`
	}
	if err = c.api(ctx, http.MethodPost, path, query, body, &data); err != nil {
		return "", err
	}
	if !validID(data.ID) {
		return "", &APIError{Operation: "message acknowledgement", Uncertain: true}
	}
	return data.ID, nil
}
func (c *Client) Send(ctx context.Context, room core.Room, text, replyTo, threadID string) (string, error) {
	// Feishu post markdown retains multiline content without placing user text in
	// the card schema, and does not introduce a new application-level planner.
	return c.sendContent(ctx, room, "post", core.Args{"zh_cn": core.Args{"content": []any{[]any{core.Args{"tag": "md", "text": text}}}}}, replyTo, threadID)
}
func (c *Client) plainCard(ctx context.Context, room core.Room, card core.Args, replyTo, threadID string) (string, error) {
	return c.sendContent(ctx, room, "interactive", card, replyTo, threadID)
}
func (c *Client) plainUpdate(ctx context.Context, id string, card core.Args) error {
	escaped, err := escapeID(id)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(card)
	if err != nil {
		return errors.New("feishu: invalid card")
	}
	return c.api(ctx, http.MethodPatch, "/im/v1/messages/"+escaped, nil, core.Args{"content": string(encoded)}, nil)
}

type historyItem struct {
	ID         string `json:"message_id"`
	ChatID     string `json:"chat_id"`
	Type       string `json:"msg_type"`
	CreateTime string `json:"create_time"`
	Deleted    bool   `json:"deleted"`
	ParentID   string `json:"parent_id"`
	RootID     string `json:"root_id"`
	ThreadID   string `json:"thread_id"`
	Body       struct {
		Content string `json:"content"`
	} `json:"body"`
	Sender struct {
		ID        string `json:"id"`
		IDType    string `json:"id_type"`
		Type      string `json:"sender_type"`
		Name      string `json:"sender_name"`
		OpenBotID string `json:"open_bot_id"`
	} `json:"sender"`
	Mentions []struct {
		Key    string `json:"key"`
		ID     string `json:"id"`
		IDType string `json:"id_type"`
		Name   string `json:"name"`
	} `json:"mentions"`
}

func (c *Client) historyMessage(item historyItem, room core.Room) *core.Message {
	if !validID(item.ID) || item.Deleted || item.ChatID != room.ChatID || !validID(item.Sender.ID) {
		return nil
	}
	bot := item.Sender.Type != "user"
	botID := c.botOpenID()
	self := bot && (item.Sender.ID == c.config.AppID || (botID != "" && (item.Sender.ID == botID || item.Sender.OpenBotID == botID)))
	if bot && !self || !bot && !authorizedAuthor(room, item.Sender.ID) {
		return nil
	}
	event := messageEvent{}
	event.Sender.ID.OpenID = item.Sender.ID
	event.Sender.Name = item.Sender.Name
	event.Sender.Type = "user"
	if bot {
		event.Sender.Type = "app"
	}
	event.Message = eventMessage{ID: item.ID, ChatID: item.ChatID, ChatType: "group", Type: item.Type, Content: item.Body.Content, CreateTime: item.CreateTime, ParentID: item.ParentID, RootID: item.RootID, ThreadID: item.ThreadID}
	if room.DMUser != "" {
		event.Message.ChatType = "p2p"
	}
	for _, v := range item.Mentions {
		m := mention{Key: v.Key, Name: v.Name}
		if v.IDType == "open_id" {
			m.ID.OpenID = v.ID
		}
		event.Message.Mentions = append(event.Message.Mentions, m)
	}
	m := normalizeMessage(event, botID, false)
	m.SenderIsSelf = self
	return &m
}
func (c *Client) fetchItem(ctx context.Context, id string) (*historyItem, error) {
	escaped, err := escapeID(id)
	if err != nil {
		return nil, err
	}
	var data struct {
		Items []historyItem `json:"items"`
	}
	if err = c.api(ctx, http.MethodGet, "/im/v1/messages/"+escaped, url.Values{"user_id_type": {"open_id"}}, nil, &data); err != nil {
		return nil, err
	}
	for _, item := range data.Items {
		if item.ID == id {
			return &item, nil
		}
	}
	return nil, nil
}
func (c *Client) Fetch(ctx context.Context, id string, room core.Room) (*core.Message, error) {
	current, err := c.validatedRoom(room)
	if err != nil {
		return nil, err
	}
	item, err := c.fetchItem(ctx, id)
	if err != nil || item == nil {
		return nil, err
	}
	message := c.historyMessage(*item, current)
	if message != nil {
		c.rememberOrigin(message.ID, current)
	}
	return message, nil
}
func (c *Client) History(ctx context.Context, room core.Room, q core.HistoryQuery) (core.HistoryPage, error) {
	room, err := c.validatedRoom(room)
	if err != nil {
		return core.HistoryPage{}, err
	}
	if q.Limit < 1 || q.Limit > 50 || q.StartTime < 0 || q.EndTime < 0 || q.StartTime > 0 && q.EndTime > 0 && q.StartTime > q.EndTime || len(q.PageToken) > 4096 {
		return core.HistoryPage{}, errors.New("feishu: invalid history query")
	}
	values := url.Values{"container_id_type": {"chat"}, "container_id": {room.ChatID}, "sort_type": {"ByCreateTimeDesc"}, "page_size": {strconv.Itoa(q.Limit)}, "with_sender_name": {"true"}, "user_id_type": {"open_id"}}
	if q.ThreadID != "" {
		if !validID(q.ThreadID) {
			return core.HistoryPage{}, errors.New("feishu: invalid thread")
		}
		values.Set("container_id_type", "thread")
		values.Set("container_id", q.ThreadID)
	}
	if q.StartTime > 0 {
		values.Set("start_time", strconv.FormatInt(q.StartTime, 10))
	}
	if q.EndTime > 0 {
		values.Set("end_time", strconv.FormatInt(q.EndTime, 10))
	}
	if q.PageToken != "" {
		values.Set("page_token", q.PageToken)
	}
	var data struct {
		Items     []historyItem `json:"items"`
		HasMore   bool          `json:"has_more"`
		PageToken string        `json:"page_token"`
	}
	if err = c.api(ctx, http.MethodGet, "/im/v1/messages", values, nil, &data); err != nil {
		return core.HistoryPage{}, err
	}
	result := core.HistoryPage{Messages: []core.Message{}}
	if data.HasMore {
		if data.PageToken == "" || len(data.PageToken) > 4096 {
			return core.HistoryPage{}, errors.New("feishu: history pagination is incomplete")
		}
		result.NextPageToken = data.PageToken
	}
	for _, item := range data.Items {
		if q.ThreadID != "" && item.ThreadID != q.ThreadID {
			continue
		}
		if msg := c.historyMessage(item, room); msg != nil {
			c.rememberOrigin(msg.ID, room)
			result.Messages = append(result.Messages, *msg)
		}
	}
	return result, nil
}

func (c *Client) Download(ctx context.Context, messageID string, r core.Resource, destination string, maxBytes int64) error {
	if r.Kind != "image" && r.Kind != "file" {
		return errors.New("feishu: unsupported resource type")
	}
	if maxBytes <= 0 {
		return errors.New("feishu: attachment byte limit must be positive")
	}
	if c.config.Limits.AttachmentBytes > 0 && maxBytes > c.config.Limits.AttachmentBytes {
		maxBytes = c.config.Limits.AttachmentBytes
	}
	message, err := escapeID(messageID)
	if err != nil {
		return err
	}
	key, err := escapeID(r.Key)
	if err != nil {
		return err
	}
	// Reauthorize the source and exact attachment association before reading any
	// bytes; Download does not trust an arbitrary key supplied by a tool call.
	room, err := c.attachmentRoom(ctx, messageID)
	if err != nil {
		return err
	}
	item, err := c.fetchItem(ctx, messageID)
	if err != nil {
		return err
	}
	if item == nil || item.Deleted || item.Sender.Type != "user" {
		return errors.New("feishu: attachment origin is unavailable")
	}
	normalized := c.historyMessage(*item, room)
	if normalized == nil || normalized.InputIssue != "" {
		return errors.New("feishu: attachment origin is not readable")
	}
	found := false
	for _, attached := range normalized.Resources {
		if attached.Key == r.Key && attached.Kind == r.Kind {
			found = true
			break
		}
	}
	if !found {
		return errors.New("feishu: attachment is not present in its origin message")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/open-apis/im/v1/messages/"+message+"/resources/"+key+"?type="+url.QueryEscape(r.Kind), nil)
	if err != nil {
		return safeFailure("download", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	httpClient := *c.http
	httpClient.Timeout = 60 * time.Second
	resp, err := httpClient.Do(req)
	if err != nil {
		return safeFailure("download", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Operation: "download", Status: resp.StatusCode}
	}
	if resp.ContentLength > maxBytes {
		return errors.New("feishu: attachment exceeds byte limit")
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("feishu: attachment destination is unavailable")
	}
	success := false
	defer func() {
		_ = out.Close()
		if !success {
			_ = os.Remove(destination)
		}
	}()
	count, err := io.Copy(out, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return safeFailure("download", err)
	}
	if count > maxBytes {
		return errors.New("feishu: attachment exceeds byte limit")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err = out.Close(); err != nil {
		return errors.New("feishu: attachment write failed")
	}
	success = true
	return nil
}

func (c *Client) File(ctx context.Context, room core.Room, path, name string) (string, error) {
	if _, err := c.validatedRoom(room); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("feishu: output file is unavailable")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return "", errors.New("feishu: output must be a regular file")
	}
	limit := c.config.Limits.AttachmentBytes
	if limit <= 0 || stat.Size() > limit {
		return "", errors.New("feishu: output file exceeds byte limit")
	}
	if name == "" {
		name = filepath.Base(path)
	}
	if !validID(name) || strings.ContainsAny(name, "/\\") {
		return "", errors.New("feishu: invalid output filename")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// io.Pipe keeps uploads bounded as well as downloads. No whole-file buffer or
	// transparent replay is possible for this stream.
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		e := multipartWriter.WriteField("file_type", "stream")
		if e == nil {
			e = multipartWriter.WriteField("file_name", name)
		}
		if e == nil {
			var part io.Writer
			part, e = multipartWriter.CreateFormFile("file", name)
			if e == nil {
				var n int64
				n, e = io.Copy(part, io.LimitReader(file, limit+1))
				if n > limit {
					e = errors.New("file grew beyond byte limit")
				}
			}
		}
		if e == nil {
			e = multipartWriter.Close()
		}
		_ = writer.CloseWithError(e)
		done <- e
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/open-apis/im/v1/files", reader)
	if err != nil {
		_ = reader.Close()
		<-done
		return "", safeFailure("upload", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	var data struct {
		Key string `json:"file_key"`
	}
	err = c.doAPI(req, &data, true)
	_ = reader.Close()
	uploadErr := <-done
	if err != nil {
		return "", err
	}
	if uploadErr != nil || !validID(data.Key) {
		return "", &APIError{Operation: "upload acknowledgement", Uncertain: true}
	}
	return c.sendContent(ctx, room, "file", core.Args{"file_key": data.Key}, "", "")
}

func formatSafeError(err error) core.Args {
	var e *APIError
	if errors.As(err, &e) {
		return core.Args{"code": e.Code, "status": e.Status, "uncertain": e.Uncertain}
	}
	if errors.Is(err, context.Canceled) {
		return core.Args{"category": "cancelled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.Args{"category": "timeout"}
	}
	return core.Args{"category": "transport"}
}

func (c *Client) rememberOrigin(id string, room core.Room) {
	c.originMu.Lock()
	defer c.originMu.Unlock()
	if len(c.origins) >= 512 {
		for key := range c.origins {
			delete(c.origins, key)
			break
		}
	}
	c.origins[id] = room
}
func (c *Client) attachmentRoom(ctx context.Context, id string) (core.Room, error) {
	c.originMu.Lock()
	room, ok := c.origins[id]
	c.originMu.Unlock()
	if ok {
		return c.validatedRoom(room)
	}
	if c.db != nil {
		// Restart recovery uses normalized, scoped records already accepted by the
		// gateway. It never treats an allowlisted DM user as authority for any group.
		var key string
		err := c.db.QueryRowContext(ctx, "SELECT room FROM context_refs WHERE id=? UNION ALL SELECT room FROM inbox WHERE id=? LIMIT 1", id, id).Scan(&key)
		if err == nil {
			var parts []any
			if json.Unmarshal([]byte(key), &parts) == nil && len(parts) == 6 {
				chat, _ := parts[4].(string)
				return c.validatedRoom(core.Room{Key: key, ChatID: chat})
			}
		}
	}
	return core.Room{}, errors.New("feishu: attachment requires a previously authorized origin")
}

func (c *Client) checkReplyOrigin(ctx context.Context, id string, room core.Room) error {
	// A just-received origin is already scoped. Do not require a separate history
	// permission merely to reply to an event the bot was authorized to receive.
	if known, err := c.attachmentRoom(ctx, id); err == nil {
		if known.Key != room.Key {
			return errors.New("feishu: reply origin is outside the room scope")
		}
		return nil
	}
	origin, err := c.Fetch(ctx, id, room)
	if err != nil {
		return err
	}
	if origin == nil {
		return errors.New("feishu: reply origin is outside the room scope")
	}
	return nil
}
