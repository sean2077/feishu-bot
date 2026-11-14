//go:build !windows

package native

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendInboxUnix(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "inbox.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			received <- ""
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		received <- string(data)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = SendInbox(ctx, socket, "token-value", "topic message"); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-received:
		if !strings.Contains(data, `"type":"auth"`) || !strings.Contains(data, `"content":"topic message"`) {
			t.Fatalf("received frames: %s", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inbox frames were not received")
	}
}
