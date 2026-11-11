package config

import (
	"encoding/json"
	"testing"
)

func TestTopicHistoryGroupConfiguration(t *testing.T) {
	v := fixture(t)
	group := v["groups"].([]any)[0].(map[string]any)
	data, _ := json.Marshal(v)
	c, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Groups[0].TopicHistory != "topic" {
		t.Fatalf("default topicHistory = %q", c.Groups[0].TopicHistory)
	}
	if r, ok := c.RoomFor("oc_example_group", ""); !ok || r.TopicHistory != "topic" {
		t.Fatalf("room topicHistory = %+v", r)
	}

	group["sessionScope"] = "thread"
	group["topicHistory"] = "chat"
	data, _ = json.Marshal(v)
	c, err = Parse(data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Groups[0].TopicHistory != "chat" {
		t.Fatal("chat scope was not retained")
	}
	if r, ok := c.RoomFor("oc_example_group", ""); !ok || r.TopicHistory != "chat" {
		t.Fatalf("chat room topicHistory = %+v", r)
	}

	group["sessionScope"] = "chat"
	data, _ = json.Marshal(v)
	if _, err := Parse(data, t.TempDir()); err == nil {
		t.Fatal("topicHistory=chat on a chat-scoped group was accepted")
	}

	group["sessionScope"] = "thread"
	group["topicHistory"] = "group"
	data, _ = json.Marshal(v)
	if _, err := Parse(data, t.TempDir()); err == nil {
		t.Fatal("invalid topicHistory value was accepted")
	}
}
