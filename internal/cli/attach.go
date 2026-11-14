package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

const attachHelp = `Adopt the current local Claude Code session into a Feishu DM topic.

Usage:
  feishu-bot attach --config PATH [--user OPEN_ID] [--title TEXT]
                    [--summary-file PATH|-] [--native ID] [--holder-pid PID]
                    [--name NAME] [--json]

Defaults come from the running Claude Code environment:
  --native     $CLAUDE_CODE_SESSION_ID
  --holder-pid $CLAUDE_PID
The session display name is read from
$CLAUDE_CONFIG_DIR/sessions/<holder-pid>.json only when its sessionId matches
the native ID; --name overrides it.
The working directory is read from the native transcript below the inherited
Claude configuration directory, never from the shell's current directory. The
gateway must already be running; this command never starts it.

The summary is written by the agent and appears in the first topic reply. The
topic is created in the configured DM. Only the DM owner may use it. On
Windows, Git Bash rewrites arguments that start with '/': set
MSYS_NO_PATHCONV=1 before passing such text, for example a title.
`

const (
	attachRequestDir     = "attach"
	attachSummaryLimit   = 256 << 10
	attachResultLimit    = 64 << 10
	attachWait           = 30 * time.Second
	attachResultPollTime = 150 * time.Millisecond
)

func parseNonNegativePID(value string) (int, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || pid < 0 {
		return 0, errors.New("holder PID must be a non-negative integer")
	}
	return pid, nil
}

func attachTarget(c core.Config, o Options) (string, error) {
	if o.AttachUser != "" {
		if !c.AdoptEnabled(o.AttachUser) {
			return "", errors.New("--user does not name a DM with adoptRoots enabled")
		}
		return o.AttachUser, nil
	}
	users := []string{}
	for _, dm := range c.DMs {
		if c.AdoptEnabled(dm.UserID) {
			users = append(users, dm.UserID)
		}
	}
	if len(users) == 1 {
		return users[0], nil
	}
	if len(users) == 0 {
		return "", errors.New("no DM enables adoptRoots; configure dms[].adoptRoots first")
	}
	return "", errors.New("multiple DMs enable adoptRoots; pass --user")
}

func readAttachSummary(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	var reader io.Reader
	var file *os.File
	if path == "-" {
		reader = os.Stdin
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", errors.New("invalid summary file")
		}
		file, err = os.Open(absolute)
		if err != nil {
			return "", errors.New("cannot read summary file")
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, attachSummaryLimit+1))
	if err != nil {
		return "", errors.New("cannot read summary file")
	}
	if len(data) > attachSummaryLimit {
		return "", errors.New("summary file exceeds 256 KiB")
	}
	return string(data), nil
}

func clipRunes(text string, limit int) string {
	value := []rune(text)
	if len(value) <= limit {
		return text
	}
	return string(value[:limit])
}

func gatewayRunning(dataDir string) error {
	lock, err := workspace.ReadLock(dataDir)
	if err != nil {
		return errors.New("the gateway is not running; start it before adopting a session")
	}
	if !process.Alive(lock.PID) {
		return fmt.Errorf("gateway.lock records PID %d, which is not running; inspect the lock before adopting", lock.PID)
	}
	return nil
}

func writeAttachRequest(dataDir string, req core.AttachRequest) (string, error) {
	return writeControlRequest(dataDir, req.ID, req)
}

func waitAttachResult(ctx context.Context, dataDir, id string) (core.AttachResult, bool, error) {
	var result core.AttachResult
	ok, err := waitControlResult(ctx, dataDir, id, &result)
	return result, ok, err
}

func withdrawAttachRequest(dataDir, id string) bool {
	return withdrawControlRequest(dataDir, id)
}

func printAttachResult(out io.Writer, o Options, result core.AttachResult) error {
	if o.JSON {
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, string(data)); err != nil {
			return err
		}
	} else {
		if result.OK {
			_, _ = fmt.Fprintf(out, "Adopted into a Feishu DM topic.\nChat: %s\nRoot message: %s\nThread: %s\n", result.ChatID, result.RootMessageID, result.ThreadID)
			if result.SessionName != "" {
				_, _ = fmt.Fprintf(out, "Session: %s\n", result.SessionName)
			}
			if result.TopicURL != "" {
				_, _ = fmt.Fprintf(out, "Topic: %s\n", result.TopicURL)
			}
		} else {
			_, _ = fmt.Fprintf(out, "Adoption not confirmed (state: %s).\n", valueOr(result.State, "unknown"))
			if result.ChatID != "" {
				_, _ = fmt.Fprintf(out, "Chat: %s\n", result.ChatID)
			}
			if result.RootMessageID != "" {
				_, _ = fmt.Fprintf(out, "Root message: %s\n", result.RootMessageID)
			}
		}
		for _, warning := range result.Warnings {
			_, _ = fmt.Fprintf(out, "Warning: %s\n", warning)
		}
	}
	if !result.OK {
		if result.Error != "" {
			return errors.New(result.Error)
		}
		return errors.New("adoption not confirmed")
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func runAttach(ctx context.Context, c core.Config, o Options, out io.Writer) error {
	user, err := attachTarget(c, o)
	if err != nil {
		return err
	}
	nativeID := o.AttachNative
	if nativeID == "" {
		nativeID = strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	}
	if nativeID == "" {
		return errors.New("not running inside Claude Code; pass --native")
	}
	holderPID := o.AttachHolderPID
	if !o.AttachHolderSet {
		raw := strings.TrimSpace(os.Getenv("CLAUDE_PID"))
		if raw == "" {
			return errors.New("not running inside Claude Code; pass --holder-pid")
		}
		holderPID, err = parseNonNegativePID(raw)
		if err != nil {
			return err
		}
	}
	holderStarted := int64(0)
	if holderPID > 0 {
		if started, ok := process.StartTime(holderPID); ok {
			holderStarted = started
		}
	}
	if err = gatewayRunning(c.DataDir); err != nil {
		return err
	}
	_, configDir, _, err := workspace.InheritedConfig()
	if err != nil {
		return err
	}
	transcript, err := native.LocateTranscript(configDir, nativeID)
	if err != nil {
		return err
	}
	cwd, err := workspace.PhysicalDirectory(transcript.CWD)
	if err != nil {
		return errors.New("local session working directory is not available")
	}
	if !native.SameProjectDir(native.EncodeProjectDir(cwd), filepath.Base(filepath.Dir(transcript.Path))) {
		return errors.New("local session physical working directory does not match its transcript project directory (symlink or case difference); restart Claude Code from the physical path before adopting")
	}
	if !c.AdoptWorkAllowed(user, cwd) {
		return errors.New("local session working directory is outside the configured adoptRoots")
	}
	summary, err := readAttachSummary(o.AttachSummaryFile)
	if err != nil {
		return err
	}
	name := adoptSessionName(o.AttachName, configDir, holderPID, nativeID)
	req := core.AttachRequest{
		Kind:            core.ControlKindAttach,
		ID:              core.NewID(),
		At:              time.Now().UnixMilli(),
		User:            user,
		Native:          nativeID,
		CWD:             cwd,
		HolderPID:       holderPID,
		HolderStarted:   holderStarted,
		Title:           strings.Join(strings.Fields(o.AttachTitle), " "),
		SessionName:     name,
		Summary:         clipRunes(summary, core.AdoptSummaryRunes),
		ConfigPath:      o.ConfigPath,
		MessagingSocket: strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")),
		MessagingToken:  os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"),
	}
	if _, err = writeAttachRequest(c.DataDir, req); err != nil {
		return err
	}
	result, confirmed, err := waitAttachResult(ctx, c.DataDir, req.ID)
	if err != nil {
		return err
	}
	if !confirmed {
		if withdrawAttachRequest(c.DataDir, req.ID) {
			return errors.New("adoption not confirmed: the request was withdrawn before processing; no topic was created.")
		}
		return errors.New("adoption not confirmed: the gateway did not answer within 30 seconds; it may still process the request. Check Feishu; do not resend.")
	}
	return printAttachResult(out, o, result)
}
