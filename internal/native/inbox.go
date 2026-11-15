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

// InboxOutcome classifies a native inbox submission. The receiver does not
// acknowledge acceptance, so a complete write is submitted rather than
// processed, and a write interrupted after it started is uncertain.
type InboxOutcome int

const (
	InboxNotSent InboxOutcome = iota
	InboxSubmitted
	InboxUncertain
)

func inboxWriteOutcome(n, total int, err error) (InboxOutcome, error) {
	if n >= total {
		return InboxSubmitted, nil
	}
	if n > 0 {
		return InboxUncertain, errors.New(errInboxSubmission)
	}
	return InboxNotSent, errors.New(errInboxSubmission)
}

type inboxOpenResult struct {
	file *os.File
	err  error
}

func closeLateInboxFile(opened <-chan inboxOpenResult) {
	result := <-opened
	if result.file != nil {
		_ = result.file.Close()
	}
}

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
// identified by socket. It reports whether no bytes were written, a complete
// frame was submitted, or the outcome is uncertain. The receiver does not
// acknowledge acceptance. The token is never included in returned errors.
func SendInbox(ctx context.Context, socket, token, text string) (InboxOutcome, error) {
	if strings.TrimSpace(socket) == "" {
		return InboxNotSent, errors.New("native inbox socket is missing")
	}
	if runtime.GOOS == "windows" && token == "" {
		return InboxNotSent, errors.New("native inbox token is missing")
	}
	if err := ctx.Err(); err != nil {
		return InboxNotSent, err
	}
	wire, err := inboxFrames(token, text)
	if err != nil {
		return InboxNotSent, err
	}
	if runtime.GOOS == "windows" {
		return sendInboxPipe(ctx, socket, wire)
	}
	dialer := net.Dialer{Timeout: inboxWriteTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return InboxNotSent, errors.New(errInboxSubmission)
	}
	defer conn.Close()
	if err = conn.SetWriteDeadline(time.Now().Add(inboxWriteTimeout)); err != nil {
		return InboxNotSent, errors.New(errInboxSubmission)
	}
	n, err := conn.Write(wire)
	return inboxWriteOutcome(n, len(wire), err)
}

// sendInboxPipe opens a native Windows named pipe with CreateFile semantics.
// Both CreateFile and the write can block until a server instance accepts, so
// a watchdog bounds each phase. A handle that arrives after the caller gave up
// is owned and closed by closeLateInboxFile.
func sendInboxPipe(ctx context.Context, socket string, wire []byte) (InboxOutcome, error) {
	opened := make(chan inboxOpenResult, 1)
	go func() {
		file, err := os.OpenFile(socket, os.O_RDWR, 0)
		opened <- inboxOpenResult{file: file, err: err}
	}()
	var file *os.File
	openTimer := time.NewTimer(inboxWriteTimeout)
	select {
	case result := <-opened:
		openTimer.Stop()
		if result.err != nil {
			return InboxNotSent, errors.New(errInboxSubmission)
		}
		file = result.file
	case <-ctx.Done():
		openTimer.Stop()
		go closeLateInboxFile(opened)
		return InboxNotSent, ctx.Err()
	case <-openTimer.C:
		go closeLateInboxFile(opened)
		return InboxNotSent, fmt.Errorf("native inbox open timed out after %s", inboxWriteTimeout)
	}
	type writeResult struct {
		n        int
		writeErr error
	}
	done := make(chan writeResult, 1)
	go func() {
		n, writeErr := file.Write(wire)
		// A complete write is submitted even if close reports an error.
		_ = file.Close()
		done <- writeResult{n: n, writeErr: writeErr}
	}()
	timer := time.NewTimer(inboxWriteTimeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return inboxWriteOutcome(result.n, len(wire), result.writeErr)
	case <-ctx.Done():
		_ = file.Close()
		return InboxUncertain, ctx.Err()
	case <-timer.C:
		_ = file.Close()
		return InboxUncertain, fmt.Errorf("native inbox write timed out after %s", inboxWriteTimeout)
	}
}
