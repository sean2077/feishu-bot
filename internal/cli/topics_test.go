package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func TestTopicsListsAdoptions(t *testing.T) {
	root := t.TempDir()
	c := attachConfig(t, root)
	native := "11111111-2222-3333-4444-555555555555"
	pendingNative := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	room, ok := c.AdoptedTopicRoom("oc_dm", "ou_dm", "om_root")
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	st, err := store.Open(filepath.Join(c.DataDir, "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.BeginAdoption(native, "ou_dm", root, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = st.SetAdoptionIdentity(native, "example-session-82", "handoff"); err != nil {
		t.Fatal(err)
	}
	if err = st.BindAdoptionRoot(native, room.Key, "om_root", "session-one", "/config", false, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err = st.OpenAdoption(native, "omt_topic"); err != nil {
		t.Fatal(err)
	}
	if err = st.UpdateAdoptionHolder(native, os.Getpid(), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.BeginAdoption(pendingNative, "ou_dm", root, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err = runTopics(c, Options{JSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	var listing topicListing
	if err = json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatalf("json: %v; %s", err, out.String())
	}
	if len(listing.Topics) != 2 {
		t.Fatalf("topics = %+v", listing.Topics)
	}
	byNative := map[string]topicInfo{}
	for _, topic := range listing.Topics {
		byNative[topic.Native] = topic
	}
	open := byNative[native]
	if open.Name != "example-session-82" || open.Thread != "omt_topic" || open.Root != "om_root" || open.State != store.AdoptedOpen || !open.HolderAlive || open.TopicURL == "" {
		t.Fatalf("open topic: %+v", open)
	}
	pending := byNative[pendingNative]
	if pending.Name != core.ShortNative(pendingNative) || pending.State != store.AdoptedPending || pending.HolderAlive {
		t.Fatalf("pending topic: %+v", pending)
	}

	out.Reset()
	if err = runTopics(c, Options{JSON: true, TopicsNative: native}, &out); err != nil {
		t.Fatal(err)
	}
	listing = topicListing{}
	if err = json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Topics) != 1 || listing.Topics[0].Native != native {
		t.Fatalf("filtered topics = %+v", listing.Topics)
	}

	out.Reset()
	if err = runTopics(c, Options{}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "example-session-82") || !strings.Contains(out.String(), "holder=yes") {
		t.Fatalf("text topics: %s", out.String())
	}
}

func TestTopicsMissingState(t *testing.T) {
	root := t.TempDir()
	c := attachConfig(t, root)
	if err := runTopics(c, Options{}, &bytes.Buffer{}); err == nil {
		t.Fatal("topics accepted a missing state database")
	}
}
