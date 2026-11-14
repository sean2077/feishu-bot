package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"
)

const inboxWriteTimeout = 5 * time.Second

const errInboxSubmission = "native inbox submission failed"

// inboxFrames builds the two newline-delimited JSON frames used by the native
// Claude Code cross-session inbox. The token is optional on Unix and required
// on native Windows.
func inboxFrames(token, text string) ([]byte, error) {
	if strings.ContainsRune(text, 0) {
		return nil, errors.New("native inbox text contains NUL")
	}
	frames := make([]any, 0, 2)
	if token != "" {
		frames = append(frames, map[string]any{"type": "auth", "token": token})
	}
	frames = append(frames, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	var out strings.Builder
	for _, frame := range frames {
		encoded, err := json.Marshal(frame)
		if err != nil {
			return nil, errors.New("native inbox frame encoding failed")
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return []byte(out.String()), nil
}

// SendInbox submits one ordinary user message to the live Claude Code process
// identified by socket. It reports submission only: the receiver does not
// acknowledge acceptance. The token is never included in returned errors.
func SendInbox(ctx context.Context, socket, token, text string) error {
	if strings.TrimSpace(socket) == "" {
		return errors.New("native inbox socket is missing")
	}
	if runtime.GOOS == "windows" && token == "" {
		return errors.New("native inbox token is missing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	wire, err := inboxFrames(token, text)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return sendInboxPipe(ctx, socket, wire)
	}
	dialer := net.Dialer{Timeout: inboxWriteTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return errors.New(errInboxSubmission)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(inboxWriteTimeout))
	if _, err = conn.Write(wire); err != nil {
		return errors.New(errInboxSubmission)
	}
	return nil
}

// sendInboxPipe opens a native Windows named pipe with CreateFile semantics.
// Both CreateFile and the write can block until a server instance accepts, so
// a watchdog bounds each phase.
func sendInboxPipe(ctx context.Context, socket string, wire []byte) error {
	type openResult struct {
		file *os.File
		err  error
	}
	opened := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(socket, os.O_RDWR, 0)
		opened <- openResult{file: file, err: err}
	}()
	var file *os.File
	openTimer := time.NewTimer(inboxWriteTimeout)
	select {
	case result := <-opened:
		openTimer.Stop()
		if result.err != nil {
			return errors.New(errInboxSubmission)
		}
		file = result.file
	case <-ctx.Done():
		openTimer.Stop()
		return ctx.Err()
	case <-openTimer.C:
		return fmt.Errorf("native inbox open timed out after %s", inboxWriteTimeout)
	}
	done := make(chan error, 1)
	go func() {
		_, writeErr := file.Write(wire)
		closeErr := file.Close()
		if writeErr != nil {
			writeErr = errors.New(errInboxSubmission)
		} else if closeErr != nil {
			writeErr = errors.New(errInboxSubmission)
		}
		done <- writeErr
	}()
	timer := time.NewTimer(inboxWriteTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = file.Close()
		return ctx.Err()
	case <-timer.C:
		_ = file.Close()
		return fmt.Errorf("native inbox write timed out after %s", inboxWriteTimeout)
	}
}
