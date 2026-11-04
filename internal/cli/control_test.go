package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestStopControlFileShutsDownCleanly(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	channel := &fakeChannel{}
	gateway := &fakeGateway{}
	channel.start = func(ctx context.Context) error {
		if err := os.WriteFile(stopRequestPath(c.DataDir), []byte("{}\n"), 0600); err != nil {
			t.Error(err)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	err := runService(context.Background(), c, func(string, core.Args) {}, func(_ core.Config, st *store.Store, _ core.Logger) (serviceGateway, serviceChannel, error) {
		gateway.st = st
		return gateway, channel, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !channel.started || !channel.closed || !gateway.closed || gateway.st.IsOpen() {
		t.Fatal("stop request did not close the service in order")
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "gateway.lock")); !os.IsNotExist(err) {
		t.Fatal("clean stop retained the exclusive lock")
	}
}

func TestStopControlFileSuppressesTransportError(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	channel := &fakeChannel{}
	gateway := &fakeGateway{}
	channel.start = func(ctx context.Context) error {
		if err := os.WriteFile(stopRequestPath(c.DataDir), []byte("{}\n"), 0600); err != nil {
			t.Error(err)
		}
		<-ctx.Done()
		return errors.New("transport closed while stopping")
	}
	err := runService(context.Background(), c, func(string, core.Args) {}, func(_ core.Config, st *store.Store, _ core.Logger) (serviceGateway, serviceChannel, error) {
		gateway.st = st
		return gateway, channel, nil
	})
	if err != nil {
		t.Fatalf("requested stop surfaced a transport error: %v", err)
	}
	if !channel.closed || !gateway.closed || gateway.st.IsOpen() {
		t.Fatal("service was not closed")
	}
}

func TestStaleStopFileDoesNotStopService(t *testing.T) {
	c := config.Defaults()
	c.DataDir = t.TempDir()
	if err := os.WriteFile(stopRequestPath(c.DataDir), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	channel := &fakeChannel{start: func(context.Context) error {
		time.Sleep(400 * time.Millisecond)
		return nil
	}}
	gateway := &fakeGateway{}
	err := runService(context.Background(), c, func(string, core.Args) {}, func(_ core.Config, st *store.Store, _ core.Logger) (serviceGateway, serviceChannel, error) {
		gateway.st = st
		return gateway, channel, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !channel.started || !channel.closed || !gateway.closed || gateway.st.IsOpen() {
		t.Fatal("service lifecycle was not completed")
	}
	if _, err := os.Stat(stopRequestPath(c.DataDir)); !os.IsNotExist(err) {
		t.Fatal("stale stop request was not removed")
	}
}
