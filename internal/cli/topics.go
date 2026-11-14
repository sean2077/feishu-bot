package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

const topicsHelp = `List local Claude Code sessions adopted into Feishu DM topics.

Usage:
  feishu-bot topics [--config PATH] [--native ID] [--json]

The listing is read-only and works while the gateway is running. name falls
back to the first eight native ID characters. holder_alive reports whether the
registered local terminal process still owns the session.
`

type topicInfo struct {
	Name        string `json:"name"`
	Native      string `json:"native"`
	Thread      string `json:"thread"`
	Root        string `json:"root"`
	State       string `json:"state"`
	HolderAlive bool   `json:"holder_alive"`
	TopicURL    string `json:"topic_url,omitempty"`
}

type topicListing struct {
	Topics []topicInfo `json:"topics"`
}

func runTopics(c core.Config, o Options, out io.Writer) error {
	path := filepath.Join(c.DataDir, "gateway.sqlite")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return errors.New("existing gateway state is required for topics")
	}
	st, err := store.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer st.Close()
	records, err := st.AdoptedTopics()
	if err != nil {
		return err
	}
	topics := []topicInfo{}
	for _, record := range records {
		if o.TopicsNative != "" && record.Native != o.TopicsNative {
			continue
		}
		name := record.SessionName
		if name == "" {
			name = core.ShortNative(record.Native)
		}
		chatID, _, _, _ := core.ParseRoomKey(record.Room)
		topics = append(topics, topicInfo{
			Name:        name,
			Native:      record.Native,
			Thread:      record.Thread,
			Root:        record.Root,
			State:       record.State,
			HolderAlive: record.HolderPID > 0 && adoptedSessionHeld(record),
			TopicURL:    core.TopicAppLink(chatID, record.Thread),
		})
	}
	if o.JSON {
		data, err := json.MarshalIndent(topicListing{Topics: topics}, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	if len(topics) == 0 {
		_, err = fmt.Fprintln(out, "No adopted topics.")
		return err
	}
	for _, topic := range topics {
		holder := "no"
		if topic.HolderAlive {
			holder = "yes"
		}
		if _, err = fmt.Fprintf(out, "%s\t%s\t%s\t%s\tholder=%s\n", topic.Name, topic.Native, topic.Thread, topic.State, holder); err != nil {
			return err
		}
		if topic.TopicURL != "" {
			if _, err = fmt.Fprintln(out, "  "+topic.TopicURL); err != nil {
				return err
			}
		}
	}
	return nil
}
