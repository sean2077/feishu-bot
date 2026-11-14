package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

const topicTextLimit = 256 << 10

func notifyResultExit(result core.NotifyResult) int {
	if result.OK {
		return 0
	}
	if result.State == core.AdoptionPending || result.State == core.AdoptionUncertain {
		return 2
	}
	if result.Error == nil {
		return 5
	}
	switch result.Error.Code {
	case core.NotifyErrorNotConfigured:
		return 3
	case core.NotifyErrorGatewayUnavailable:
		return 4
	case core.NotifyErrorTransportFailure:
		return 6
	default:
		return 5
	}
}

func notifyErrorResult(code, message string) core.NotifyResult {
	return core.NotifyResult{Error: &core.AdoptError{Code: code, Message: message}}
}

func printNotifyResult(out io.Writer, o Options, result core.NotifyResult) {
	if o.JSON {
		data, err := json.MarshalIndent(result, "", "  ")
		if err == nil {
			_, _ = fmt.Fprintln(out, string(data))
		}
		return
	}
	if result.OK {
		_, _ = fmt.Fprintf(out, "Notification delivered.\nChat: %s\nRoot: %s\nThread: %s\nMessage: %s\nCreated: %t\n", result.ChatID, result.RootMessageID, result.ThreadID, result.MessageID, result.Created)
		if result.SessionName != "" {
			_, _ = fmt.Fprintf(out, "Session: %s\n", result.SessionName)
		}
		if result.TopicURL != "" {
			_, _ = fmt.Fprintf(out, "Topic: %s\n", result.TopicURL)
		}
	} else {
		state := result.State
		if state == "" {
			state = "error"
		}
		_, _ = fmt.Fprintf(out, "Notification not confirmed (state: %s).\n", state)
		if result.RootMessageID != "" {
			_, _ = fmt.Fprintf(out, "Root: %s\n", result.RootMessageID)
		}
		if result.Error != nil {
			_, _ = fmt.Fprintf(out, "Error: %s: %s\n", result.Error.Code, result.Error.Message)
		}
	}
	for _, warning := range result.Warnings {
		_, _ = fmt.Fprintf(out, "Warning: %s\n", warning)
	}
}

func readTopicText(path string) (string, error) {
	if path == "" {
		return "", errors.New("topic reply requires --text-file")
	}
	var reader io.Reader
	if path == "-" {
		reader = os.Stdin
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", errors.New("invalid text file")
		}
		file, err := os.Open(absolute)
		if err != nil {
			return "", errors.New("cannot read text file")
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, topicTextLimit+1))
	if err != nil {
		return "", errors.New("cannot read text file")
	}
	if len(data) > topicTextLimit {
		return "", errors.New("text file exceeds 256 KiB")
	}
	return string(data), nil
}

func runNotify(ctx context.Context, c core.Config, o Options, out io.Writer) error {
	user, err := attachTarget(c, o)
	if err != nil {
		result := notifyErrorResult(core.NotifyErrorNotConfigured, err.Error())
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 3}
	}
	nativeID := o.AttachNative
	if nativeID == "" {
		nativeID = strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	}
	if nativeID == "" {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, "not running inside Claude Code; pass --native")
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	holderPID := o.AttachHolderPID
	if !o.AttachHolderSet {
		raw := strings.TrimSpace(os.Getenv("CLAUDE_PID"))
		if raw == "" {
			result := notifyErrorResult(core.NotifyErrorInvalidRequest, "not running inside Claude Code; pass --holder-pid")
			printNotifyResult(out, o, result)
			return &exitCodeError{code: 5}
		}
		holderPID, err = parseNonNegativePID(raw)
		if err != nil {
			result := notifyErrorResult(core.NotifyErrorInvalidRequest, err.Error())
			printNotifyResult(out, o, result)
			return &exitCodeError{code: 5}
		}
	}
	holderStarted := int64(0)
	if holderPID > 0 {
		if started, ok := process.StartTime(holderPID); ok {
			holderStarted = started
		}
	}
	if err = gatewayRunning(c.DataDir); err != nil {
		result := notifyErrorResult(core.NotifyErrorGatewayUnavailable, err.Error())
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 4}
	}
	_, configDir, _, err := workspace.InheritedConfig()
	if err != nil {
		result := notifyErrorResult(core.NotifyErrorNotConfigured, err.Error())
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 3}
	}
	transcript, err := native.LocateTranscript(configDir, nativeID)
	if err != nil {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, err.Error())
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	cwd, err := workspace.PhysicalDirectory(transcript.CWD)
	if err != nil {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, "local session working directory is not available")
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	if !native.SameProjectDir(native.EncodeProjectDir(cwd), filepath.Base(filepath.Dir(transcript.Path))) {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, "local session physical working directory does not match its transcript project directory")
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	if !c.AdoptWorkAllowed(user, cwd) {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, "local session working directory is outside the configured adoptRoots")
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	summary, err := readAttachSummary(o.AttachSummaryFile)
	if err != nil {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, err.Error())
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	title := strings.Join(strings.Fields(o.AttachTitle), " ")
	if strings.TrimSpace(summary) == "" && title == "" {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, "notify requires a non-empty summary")
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	name := adoptSessionName(o.AttachName, configDir, holderPID, nativeID)
	req := core.NotifyRequest{
		Kind:            core.ControlKindNotify,
		ID:              core.NewID(),
		At:              time.Now().UnixMilli(),
		User:            user,
		Native:          nativeID,
		CWD:             cwd,
		HolderPID:       holderPID,
		HolderStarted:   holderStarted,
		Title:           title,
		SessionName:     name,
		Summary:         clipRunes(summary, core.AdoptSummaryRunes),
		ConfigPath:      o.ConfigPath,
		MessagingSocket: strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")),
		MessagingToken:  os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"),
	}
	if _, err = writeControlRequest(c.DataDir, req.ID, req); err != nil {
		result := notifyErrorResult(core.NotifyErrorInvalidRequest, err.Error())
		printNotifyResult(out, o, result)
		return &exitCodeError{code: 5}
	}
	var result core.NotifyResult
	confirmed, err := waitControlResult(ctx, c.DataDir, req.ID, &result)
	if err != nil {
		printNotifyResult(out, o, notifyErrorResult(core.NotifyErrorTransportFailure, err.Error()))
		return &exitCodeError{code: 6}
	}
	if !confirmed {
		if withdrawControlRequest(c.DataDir, req.ID) {
			printNotifyResult(out, o, notifyErrorResult(core.NotifyErrorTransportFailure, "notification request withdrawn before processing"))
		} else {
			printNotifyResult(out, o, notifyErrorResult(core.NotifyErrorTransportFailure, "notification result unconfirmed; check Feishu and do not resend"))
		}
		return &exitCodeError{code: 6}
	}
	printNotifyResult(out, o, result)
	return &exitCodeError{code: notifyResultExit(result)}
}

func runTopicReply(ctx context.Context, c core.Config, o Options, out io.Writer) error {
	fail := func(code int, message string) error {
		result := core.TopicReplyResult{Error: &core.AdoptError{Code: messageCode(code), Message: message}}
		if o.JSON {
			data, _ := json.MarshalIndent(result, "", "  ")
			_, _ = fmt.Fprintln(out, string(data))
		} else {
			_, _ = fmt.Fprintf(out, "Reply not confirmed: %s\n", message)
		}
		return &exitCodeError{code: code}
	}
	if o.AttachNative == "" || o.TopicMessageID == "" {
		return fail(5, "topic reply requires --native and --message-id")
	}
	text, err := readTopicText(o.TopicTextFile)
	if err != nil {
		return fail(5, err.Error())
	}
	if strings.TrimSpace(text) == "" {
		return fail(5, "reply text is empty")
	}
	if err = gatewayRunning(c.DataDir); err != nil {
		return fail(4, err.Error())
	}
	req := core.TopicReplyRequest{
		Kind:       core.ControlKindReply,
		ID:         core.NewID(),
		At:         time.Now().UnixMilli(),
		Native:     o.AttachNative,
		MessageID:  o.TopicMessageID,
		Text:       text,
		ConfigPath: o.ConfigPath,
	}
	if _, err = writeControlRequest(c.DataDir, req.ID, req); err != nil {
		return fail(5, err.Error())
	}
	var result core.TopicReplyResult
	confirmed, err := waitControlResult(ctx, c.DataDir, req.ID, &result)
	if err != nil || !confirmed {
		return fail(6, "reply result unconfirmed; check the topic and do not resend")
	}
	if o.JSON {
		data, _ := json.MarshalIndent(result, "", "  ")
		_, _ = fmt.Fprintln(out, string(data))
	} else if result.OK {
		_, _ = fmt.Fprintf(out, "Reply delivered: %s\n", result.MessageID)
	} else if result.Error != nil {
		_, _ = fmt.Fprintf(out, "Reply not confirmed: %s\n", result.Error.Message)
	}
	if result.OK {
		return &exitCodeError{code: 0}
	}
	if result.Error == nil {
		return &exitCodeError{code: 5}
	}
	switch result.Error.Code {
	case core.NotifyErrorGatewayUnavailable:
		return &exitCodeError{code: 4}
	case core.NotifyErrorTransportFailure:
		return &exitCodeError{code: 6}
	case core.NotifyErrorUncertain:
		return &exitCodeError{code: 6}
	case core.NotifyErrorNotConfigured:
		return &exitCodeError{code: 3}
	default:
		return &exitCodeError{code: 5}
	}
}

func messageCode(code int) string {
	switch code {
	case 3:
		return core.NotifyErrorNotConfigured
	case 4:
		return core.NotifyErrorGatewayUnavailable
	case 6:
		return core.NotifyErrorTransportFailure
	default:
		return core.NotifyErrorInvalidRequest
	}
}

const notifyHelp = `Send a task-result summary to the owner's adopted Feishu DM topic.

Usage:
  feishu-bot notify --config PATH --summary-file PATH|-
                    [--title TEXT] [--native ID] [--holder-pid PID]
                    [--user OPEN_ID] [--name NAME] [--json]

The first call creates the topic; later calls append one reply to the same
topic. Defaults come from $CLAUDE_CODE_SESSION_ID and $CLAUDE_PID; the working
directory comes from the local transcript. The session display name is read
from $CLAUDE_CONFIG_DIR/sessions/<holder-pid>.json only when its sessionId
matches the native ID; --name overrides it. Exit codes: 0 ok, 2 pending or
uncertain (do not resend), 3 not configured, 4 gateway unavailable, 5 invalid
request, 6 transport failure. Only 3 and 4 are safe for a legacy DM fallback.

The optional CLAUDE_CODE_MESSAGING_SOCKET/TOKEN environment registers the live
native inbox for interactive topic relay while this terminal remains open.
`

const topicHelp = `Reply to a forwarded Feishu topic message from the local session.

Usage:
  feishu-bot topic reply --config PATH --native ID --message-id om_xxx
                         --text-file -|PATH [--json]

Use the command included in the forwarded topic message. The gateway verifies
that the message belongs to this adopted topic and that it has not been replied
to already.
`
