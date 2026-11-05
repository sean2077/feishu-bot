package cli

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/feishu"
	"github.com/sean2077/feishu-bot/internal/gateway"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

type ChatVerification struct {
	Status string            `json:"status"`
	Scope  string            `json:"scope"`
	Checks map[string]string `json:"checks"`
	Detail string            `json:"detail,omitempty"`
}

type chatVerifier interface {
	Start(context.Context, func(context.Context, core.Message) error, func(context.Context, core.CardAction) (core.Args, error)) error
	Close() error
	Card(context.Context, core.Room, core.Args, string, string) (string, error)
	Update(context.Context, string, core.Args) error
	File(context.Context, core.Room, string, string) (string, error)
}

func configuredVerificationTarget(c core.Config, target string) bool {
	if user, ok := strings.CutPrefix(target, "dm:"); ok {
		if user == "" {
			return false
		}
		for _, dm := range c.DMs {
			if dm.UserID == user {
				_, found := c.Project(dm.ProjectID)
				return found
			}
		}
		return false
	}
	for _, group := range c.Groups {
		if group.ChatID == target {
			_, found := c.Project(group.ProjectID)
			return found
		}
	}
	return false
}
func verificationMessage(c core.Config, target, nonce string, m core.Message) (core.Room, bool) {
	room, ok := gateway.Route(c, m)
	if !ok || !gateway.Triggers(room, m) || strings.TrimSpace(m.Text) != "/verify "+nonce {
		return core.Room{}, false
	}
	if user, dm := strings.CutPrefix(target, "dm:"); dm {
		return room, m.ChatType == "p2p" && room.DMUser == user
	}
	return room, m.ChatType == "group" && room.ChatID == target
}
func newChatVerification() ChatVerification {
	return ChatVerification{Status: "blocked", Scope: "Explicit test-chat exchange only. No model, native tools, production session, permissions change or automatic replay. File download confirmation is the authorized user's acknowledgement.", Checks: map[string]string{"incomingMessage": "not_checked", "messageDelivery": "not_checked", "cardCallbacks": "not_checked", "fileDelivery": "not_checked", "fileDownload": "not_checked", "feishuReconnect": "not_checked"}}
}

// VerifyConfiguredChat is opt-in because it sends one card and one small file.
// The existing service lock prevents a second local event consumer. The SQLite
// state is temporary and private; the deployment database is never opened.
func VerifyConfiguredChat(parent context.Context, c core.Config, target string, out io.Writer) ChatVerification {
	r := newChatVerification()
	if !configuredVerificationTarget(c, target) {
		r.Detail = "The target must be a configured group chat ID or dm:<configured user ID>; no request was sent."
		return r
	}
	if parent.Err() != nil {
		r.Detail = "Verification cancelled; no request was sent."
		return r
	}
	unlock, err := workspace.Lock(c.DataDir)
	if err != nil {
		r.Detail = "Stop the local service before test-chat verification; the data directory lock could not be acquired."
		return r
	}
	defer unlock()
	dir, err := os.MkdirTemp("", "feishu-chat-verification-")
	if err != nil {
		r.Detail = "Cannot create private verification state."
		return r
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "verification.sqlite"))
	if err != nil {
		r.Detail = "Cannot initialize temporary verification state."
		return r
	}
	defer st.Close()
	if err = os.Chmod(filepath.Join(dir, "verification.sqlite"), 0600); err != nil {
		r.Detail = "Cannot restrict temporary verification state."
		return r
	}
	client, err := feishu.New(c, st.DB, func(string, core.Args) {})
	if err != nil {
		r.Detail = core.Redact(err.Error(), config.Secrets(c))
		return r
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	return verifyChatSession(ctx, c, target, core.NewID(), dir, out, client)
}

// Card callbacks return the response payload sent back to Feishu. Verification
// only needs a toast; the card itself is refreshed by the verification flow.
func verificationToast(text string) core.Args {
	if text == "" {
		return core.Args{}
	}
	return core.Args{"toast": core.Args{"type": "info", "content": text}}
}

func verificationCard(nonce, status string, active bool) core.Args {
	elements := []any{core.Args{"tag": "markdown", "content": status}}
	if active {
		columns := []any{}
		for _, choice := range []struct{ key, label string }{{"card", "确认看到了卡片"}, {"file", "确认已打开测试文件"}} {
			// Card JSON 2.0 declares click callbacks on the component's behaviors;
			// a top-level value is the card 1.0 form and produces no callback.
			button := core.Args{"tag": "button", "text": core.Args{"tag": "plain_text", "content": choice.label}, "type": "default", "behaviors": []any{core.Args{"type": "callback", "value": core.Args{"diagnostic": "first-connection", "nonce": nonce, "choice": choice.key}}}}
			columns = append(columns, core.Args{"tag": "column", "width": "weighted", "weight": 1, "elements": []any{button}})
		}
		elements = append(elements, core.Args{"tag": "column_set", "flex_mode": "stretch", "columns": columns})
	}
	return core.Args{"schema": "2.0", "header": core.Args{"title": core.Args{"tag": "plain_text", "content": "首次连接验证"}}, "body": core.Args{"elements": elements}}
}

func verifyChatSession(ctx context.Context, c core.Config, target, nonce, dir string, out io.Writer, client chatVerifier) (r ChatVerification) {
	r = newChatVerification()
	r.Status = "failed"
	incoming := make(chan core.Room, 1)
	completed := make(chan struct{})
	cardReady := make(chan struct{})
	var mu sync.Mutex
	var selected core.Room
	var cardID string
	var filesSent, cardConfirmed, fileConfirmed bool
	var completedOnce, readyOnce sync.Once
	ready := func() { readyOnce.Do(func() { close(cardReady) }) }
	defer ready()
	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan error, 1)
	go func() {
		stopped <- client.Start(runCtx, func(_ context.Context, m core.Message) error {
			if room, ok := verificationMessage(c, target, nonce, m); ok {
				select {
				case incoming <- room:
				default:
				}
			}
			return nil
		}, func(_ context.Context, a core.CardAction) (core.Args, error) {
			if runCtx.Err() != nil {
				return verificationToast("验证已结束"), nil
			}
			value, _ := a.Value["nonce"].(string)
			if a.Value["diagnostic"] != "first-connection" || subtle.ConstantTimeCompare([]byte(value), []byte(nonce)) != 1 {
				return verificationToast("验证请求无效"), nil
			}
			select {
			case <-cardReady:
			default:
				return verificationToast("验证卡仍在发送，请稍后确认"), nil
			}
			mu.Lock()
			defer mu.Unlock()
			if runCtx.Err() != nil {
				return verificationToast("验证已结束"), nil
			}
			if cardID == "" || a.MessageID != cardID || a.ChatID != selected.ChatID || !gateway.AuthorizedAuthor(selected, a.UserID) {
				return verificationToast("验证请求不属于当前聊天或操作者"), nil
			}
			switch a.Value["choice"] {
			case "card":
				cardConfirmed = true
			case "file":
				if !filesSent {
					return verificationToast("测试文件仍在发送，请稍后确认"), nil
				}
				fileConfirmed = true
			default:
				return verificationToast("验证操作无效"), nil
			}
			if cardConfirmed && fileConfirmed {
				completedOnce.Do(func() { close(completed) })
			}
			return verificationToast("已记录确认"), nil
		})
	}()
	defer func() {
		cancel()
		ready()
		_ = client.Close()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			r.Status = "failed"
			r.Detail = "Verification transport shutdown was not confirmed."
		}
	}()
	if out != nil {
		_, _ = fmt.Fprintf(out, "Test-chat verification is active for up to 2 minutes. In the selected authorized chat, send /verify %s (mention the bot in groups unless mentionOptional is configured). Then confirm the card and open the small test file. No model will run.\n", nonce)
	}
	select {
	case selected = <-incoming:
		r.Checks["incomingMessage"] = "passed"
	case err := <-stopped:
		stopped <- err
		r.Detail = "Feishu event connection ended before an authorized test message."
		if err != nil {
			r.Detail = core.Redact(err.Error(), config.Secrets(c))
		}
		return
	case <-ctx.Done():
		r.Detail = "No authorized test message was observed before cancellation or expiry."
		return
	}
	id, err := client.Card(ctx, selected, verificationCard(nonce, "请确认这张卡片可见，再打开随后发送的测试文件并确认。验证只在本次命令期间有效，不调用模型。", true), "", "")
	mu.Lock()
	cardID = id
	mu.Unlock()
	ready()
	if err != nil || id == "" {
		r.Checks["messageDelivery"] = "failed"
		r.Detail = "Test card delivery was not confirmed. Check the chat before retrying; unknown writes are not replayed."
		return
	}
	r.Checks["messageDelivery"] = "passed"
	defer func() {
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finishCancel()
		status := "验证已结束，按钮已失效。请查看命令行分阶段结果。"
		if r.Status == "passed" {
			status = "消息、卡片回调及测试文件已确认。模型、原生工具、重连与会话恢复仍需独立验收。"
		}
		_ = client.Update(finishCtx, id, verificationCard(nonce, status, false))
	}()
	path := filepath.Join(dir, "feishu-connection-test.txt")
	if err = os.WriteFile(path, []byte("Feishu connection verification\nThis is a harmless diagnostic file. Confirm in the test card after opening it.\n"), 0600); err != nil {
		r.Detail = "Cannot create the test file."
		return
	}
	fileID, err := client.File(ctx, selected, path, "feishu-connection-test.txt")
	if err != nil || fileID == "" {
		r.Checks["fileDelivery"] = "failed"
		r.Detail = "Test file delivery was not confirmed. Inspect the chat before explicitly retrying."
		return
	}
	mu.Lock()
	filesSent = true
	mu.Unlock()
	r.Checks["fileDelivery"] = "passed"
	_ = client.Update(ctx, id, verificationCard(nonce, "测试文件已发送。请打开文件，并分别确认卡片和文件。", true))
	select {
	case <-completed:
		r.Status = "passed"
		r.Checks["cardCallbacks"], r.Checks["fileDownload"] = "passed", "confirmed_by_user"
		r.Detail = "Authorized incoming message, card callback and file message confirmed; the authorized user confirmed opening the file. Native/model/reconnect checks were not performed."
	case err := <-stopped:
		stopped <- err
		r.Detail = "Feishu connection ended before both authorized confirmations."
	case <-ctx.Done():
		r.Detail = "Card/file confirmations were not completed before cancellation or expiry."
	}
	mu.Lock()
	if cardConfirmed || fileConfirmed {
		r.Checks["cardCallbacks"] = "passed"
	}
	if fileConfirmed {
		r.Checks["fileDownload"] = "confirmed_by_user"
	}
	mu.Unlock()
	return
}
