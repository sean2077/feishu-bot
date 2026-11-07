package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	if room.Thread != "" && replyTo == "" {
		// A topic room never posts to the chat timeline: untargeted notices,
		// status cards and files reply to the topic root inside the topic.
		replyTo = room.Thread
	}
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
		// A top-level trigger of a topic room has no thread yet; replying with
		// reply_in_thread creates the Feishu topic rooted at that message.
		body["reply_in_thread"] = threadID != "" || room.Thread != ""
	} else {
		if threadID != "" {
			return "", errors.New("feishu: thread reply requires an origin message")
		}
		body["receive_id"] = room.ChatID
	}
	ctx = c.deliveryContext(ctx)
	id, err := c.sendJournaledJSON(ctx, room.Key, "message", http.MethodPost, path, query, body, "message_id", true)
	if err == nil {
		err = c.linkDeliveryMessage(ctx, id)
	} else if room.Thread != "" && threadReplyUnsupported(err) {
		// Never fall back to a chat-timeline reply: the topic room's follow-ups
		// would then route to new topics. Report the configuration mismatch.
		c.emit("feishu_thread_reply_unsupported", core.Args{"code": apiCode(err)})
		err = fmt.Errorf("feishu: this chat does not support topic replies; use groups[].sessionScope \"chat\" for it: %w", err)
	}
	return id, err
}

// threadReplyUnsupported reports Feishu's definite refusals of a topic reply:
// 230071 (the chat does not support topic replies) and 230072 (aggregated
// messages cannot start a topic).
func threadReplyUnsupported(err error) bool {
	code := apiCode(err)
	return code == 230071 || code == 230072
}
func apiCode(err error) int {
	var failure *APIError
	if errors.As(err, &failure) && !failure.Uncertain {
		return failure.Code
	}
	return 0
}
func (c *Client) Send(ctx context.Context, room core.Room, text, replyTo, threadID string) (messageID string, resultErr error) {
	defer func() { resultErr = localRejection("message request validation", resultErr) }()
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
	maxBytes = min(maxBytes, 100<<20)
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
	// One deadline covers authorization/token reads, shared admission and all
	// body chunks; a slow preflight cannot grant the transfer another minute.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
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
	return c.downloadResource(ctx, c.base+"/open-apis/im/v1/messages/"+message+"/resources/"+key+"?type="+url.QueryEscape(r.Kind), token, destination, maxBytes)
}

func (c *Client) File(ctx context.Context, room core.Room, path, name string) (messageID string, resultErr error) {
	defer func() { resultErr = localRejection("file request validation", resultErr) }()
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
	ctx = c.deliveryContext(ctx)
	// Copy once to a private bounded snapshot. Its digest binds retries to the
	// same bytes even if the caller later replaces the original output file.
	snapshot, err := os.CreateTemp("", "feishu-upload-*")
	if err != nil {
		return "", errors.New("feishu: upload snapshot is unavailable")
	}
	defer func() { _ = snapshot.Close(); _ = os.Remove(snapshot.Name()) }()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(snapshot, digest), io.LimitReader(file, limit+1))
	if err != nil || n > limit {
		return "", errors.New("feishu: upload snapshot failed or exceeded byte limit")
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	payload := core.Args{"name": name, "size": n, "sha256": hash}
	key, err := c.deliveryStep(ctx, room.Key, "file-upload", payload, false, func(attemptCtx context.Context, frozen core.Args, _ string) (string, error) {
		if _, seekErr := snapshot.Seek(0, io.SeekStart); seekErr != nil {
			return "", errors.New("feishu: upload snapshot cannot be read")
		}
		return c.uploadFile(attemptCtx, snapshot, stringValue(frozen, "name"), "feishu_"+hash[:48], limit)
	})
	if err != nil {
		return "", err
	}
	return c.sendContent(ctx, room, "file", core.Args{"file_key": key}, "", "")
}

// uploadTimeout bounds one streamed file upload, including the request body.
const uploadTimeout = 60 * time.Second

func (c *Client) uploadFile(ctx context.Context, file io.Reader, name, boundary string, limit int64) (string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	// io.Pipe keeps uploads bounded as well as downloads. No whole-file buffer or
	// transparent replay is possible for this stream.
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	if err = multipartWriter.SetBoundary(boundary); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return "", errors.New("feishu: invalid upload boundary")
	}
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
	// The shared client's 20s total timeout would cut a streamed upload short
	// of the 60s request deadline above, so use a copy bounded by that deadline.
	client := *c.http
	client.Timeout = uploadTimeout
	err = c.doAPIWith(&client, req, &data, true)
	_ = reader.Close()
	uploadErr := <-done
	if err != nil {
		return "", err
	}
	if uploadErr != nil || !validID(data.Key) {
		return "", &APIError{Operation: "upload acknowledgement", Uncertain: true}
	}
	return data.Key, nil
}

func formatSafeError(err error) core.Args {
	var e *APIError
	if errors.As(err, &e) {
		return core.Args{"code": e.Code, "status": e.Status, "uncertain": e.Uncertain, "retryable": e.DeliveryRetryable(), "retryAfterMs": e.DeliveryRetryAfter().Milliseconds(), "requestId": e.RequestID}
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
func (c *Client) originStoredIn(ctx context.Context, id, roomKey string) bool {
	if c.db == nil {
		return false
	}
	var found int
	return c.db.QueryRowContext(ctx, "SELECT 1 FROM inbox WHERE id=? AND room=? UNION ALL SELECT 1 FROM context_refs WHERE id=? AND room=? UNION ALL SELECT 1 FROM outputs WHERE id=? AND room=? LIMIT 1", id, roomKey, id, roomKey, id, roomKey).Scan(&found) == nil
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
			if chat, _, _, ok := core.ParseRoomKey(key); ok {
				return c.validatedRoom(core.Room{Key: key, ChatID: chat})
			}
		}
	}
	return core.Room{}, errors.New("feishu: attachment requires a previously authorized origin")
}

func (c *Client) checkReplyOrigin(ctx context.Context, id string, room core.Room) error {
	room, err := c.validatedRoom(room)
	if err != nil {
		return err
	}
	if room.Thread != "" && id == room.Thread {
		// The topic root defines a topic room's key; replies stay in that topic.
		return nil
	}
	// A just-received origin is already scoped. Do not require a separate history
	// permission merely to reply to an event the bot was authorized to receive.
	if known, err := c.attachmentRoom(ctx, id); err == nil {
		// The chat room of a thread-scoped group may answer a top-level message
		// that routed to one of its topics, for example a management hint.
		if known.Key == room.Key || room.Thread == "" && known.ChatKey == room.Key {
			return nil
		}
		// The in-memory origin cache keeps one room per message. A message
		// quoted from another topic may have replaced the entry, so accept the
		// durable scoped record of this exact room before refusing.
		if c.originStoredIn(ctx, id, room.Key) {
			return nil
		}
		return errors.New("feishu: reply origin is outside the room scope")
	}
	if c.db != nil {
		var savedRoom string
		if err := c.db.QueryRowContext(ctx, "SELECT room FROM outputs WHERE id=?", id).Scan(&savedRoom); err == nil {
			// Management callbacks reply to the bot's own acknowledged card.
			// A scoped output receipt is sufficient for this reply only; it must
			// not become a history or attachment-download capability.
			if savedRoom != room.Key {
				return errors.New("feishu: reply output is outside the room scope")
			}
			_, err = c.validatedRoom(room)
			return err
		}
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
